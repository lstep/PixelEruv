package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lstep/pixeleruv/backend/internal/pb"
	"google.golang.org/protobuf/proto"
)

// TestLiteMVPFlow verifies the full pipeline:
// WS connect → AuthFrame → AuthResult → InputFrame → ReplicationBatch
//
// Prerequisites: docker compose up (worldsim, nats). The pusher is started
// in-process by TestMain (no Dex) so IdToken="dev" is accepted.
func TestLiteMVPFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect to the in-process pusher
	c, _, err := websocket.Dial(ctx, pusherAddr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Send AuthFrame
	auth := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: "dev"}}}
	authBytes, _ := proto.Marshal(auth)
	if err := c.Write(ctx, websocket.MessageBinary, authBytes); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	// Read AuthResult
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	var sf pb.ServerFrame
	if err := proto.Unmarshal(data, &sf); err != nil {
		t.Fatalf("unmarshal server frame: %v", err)
	}
	ar := sf.GetAuthResult()
	if ar == nil || !ar.Ok {
		t.Fatalf("expected auth result ok=true, got %v", sf.Payload)
	}
	t.Logf("authenticated: client=%s", ar.ClientId)

	// Send InputFrame (move right)
	input := &pb.ClientFrame{Payload: &pb.ClientFrame_Input{Input: &pb.InputFrame{
		Seq:        1,
		ClientTick: 0,
		State:      &pb.InputState{Right: true},
	}}}
	inputBytes, _ := proto.Marshal(input)
	if err := c.Write(ctx, websocket.MessageBinary, inputBytes); err != nil {
		t.Fatalf("write input: %v", err)
	}

	// Read replication batches until we see a SpawnEntity for our entity
	// or an UpdateComponent with a Position change
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("read replication: %v", err)
		}

		var sf pb.ServerFrame
		if err := proto.Unmarshal(data, &sf); err != nil {
			continue
		}
		batch := sf.GetReplication()
		if batch == nil {
			continue
		}

		t.Logf("replication batch: %d spawns, %d updates", len(batch.Spawns), len(batch.Updates))

		if len(batch.Spawns) > 0 {
			t.Logf("got SpawnEntity: %s", batch.Spawns[0].EntityId)
			return // success
		}
	}

	t.Fatal("timed out waiting for SpawnEntity")
}

// TestTwoClientsSeeEachOther verifies that a second client receives a
// SpawnEntity for the first client's entity (per-client spawn tracking).
//
// Prerequisites: docker compose up (worldsim, nats). The pusher is started
// in-process by TestMain (no Dex) so IdToken="dev" is accepted.
func TestTwoClientsSeeEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Client A connects to the in-process pusher
	cA, _, err := websocket.Dial(ctx, pusherAddr, nil)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer cA.Close(websocket.StatusNormalClosure, "")

	authA := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: "dev"}}}
	if err := cA.Write(ctx, websocket.MessageBinary, mustMarshal(authA)); err != nil {
		t.Fatalf("write auth A: %v", err)
	}
	entityA := readAuthResult(t, ctx, cA)
	t.Logf("client A: entity=%s", entityA)

	// Wait for A to receive its own spawn
	waitForSpawn(t, ctx, cA, entityA)

	// Client B connects to the in-process pusher
	cB, _, err := websocket.Dial(ctx, pusherAddr, nil)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer cB.Close(websocket.StatusNormalClosure, "")

	authB := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: "dev"}}}
	if err := cB.Write(ctx, websocket.MessageBinary, mustMarshal(authB)); err != nil {
		t.Fatalf("write auth B: %v", err)
	}
	entityB := readAuthResult(t, ctx, cB)
	t.Logf("client B: entity=%s", entityB)

	// B must receive a spawn for A's entity (the bug: B only saw itself)
	if !waitForSpawn(t, ctx, cB, entityA) {
		t.Fatalf("client B never received SpawnEntity for client A's entity %s", entityA)
	}
	t.Logf("client B saw entity A: %s", entityA)

	// A must also receive a spawn for B's entity
	if !waitForSpawn(t, ctx, cA, entityB) {
		t.Fatalf("client A never received SpawnEntity for client B's entity %s", entityB)
	}
	t.Logf("client A saw entity B: %s", entityB)
}

func readAuthResult(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	var sf pb.ServerFrame
	if err := proto.Unmarshal(data, &sf); err != nil {
		t.Fatalf("unmarshal server frame: %v", err)
	}
	ar := sf.GetAuthResult()
	if ar == nil || !ar.Ok {
		t.Fatalf("expected auth result ok=true, got %v", sf.Payload)
	}
	return ar.ClientId
}

// waitForSpawn reads replication batches until it sees a SpawnEntity for the
// given entity ID. Returns false on timeout without a fatal.
func waitForSpawn(t *testing.T, ctx context.Context, c *websocket.Conn, wantEntityID string) bool {
	t.Helper()
	// Derive entity_id from client_id the same way worldsim does: "e_" + client_id[2:]
	wantEntity := wantEntityID
	if len(wantEntityID) > 2 && wantEntityID[:2] == "c_" {
		wantEntity = "e_" + wantEntityID[2:]
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
		_, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			return false
		}
		var sf pb.ServerFrame
		if err := proto.Unmarshal(data, &sf); err != nil {
			continue
		}
		batch := sf.GetReplication()
		if batch == nil {
			continue
		}
		for _, sp := range batch.Spawns {
			if sp.EntityId == wantEntity {
				return true
			}
		}
	}
	return false
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
