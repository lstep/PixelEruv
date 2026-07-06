package integration_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/lstep/pixeleruv/backend/internal/pusher"
	"google.golang.org/protobuf/proto"
)

// startPusherWithDex starts an in-process pusher with a (fake) Dex issuer
// configured, so the "Dex configured" auth branch is exercised without
// needing a real Dex instance reachable — the guest path never calls the
// JWKS endpoint, and the invalid-token path fails fast on JWT parsing.
func startPusherWithDex(t *testing.T) string {
	t.Helper()
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv, err := pusher.New(addr, natsURL, "https://fake-issuer.test", "https://fake-issuer.test/keys", "pixeleruv",
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("pusher.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go srv.Run(ctx)
	t.Cleanup(cancel)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	return "ws://" + addr + "/ws"
}

// TestGuestAuthAllowed verifies that an empty id_token is accepted as a
// guest session when Dex is configured, rather than being rejected.
func TestGuestAuthAllowed(t *testing.T) {
	addr := startPusherWithDex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	auth := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: ""}}}
	if err := c.Write(ctx, websocket.MessageBinary, mustMarshal(auth)); err != nil {
		t.Fatalf("write auth: %v", err)
	}

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
		t.Fatalf("expected guest auth result ok=true, got %v", sf.Payload)
	}
}

// TestInvalidTokenRejected verifies that a non-empty, unparsable id_token is
// still rejected when Dex is configured (only an intentionally empty token
// is treated as a guest).
func TestInvalidTokenRejected(t *testing.T) {
	addr := startPusherWithDex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	auth := &pb.ClientFrame{Payload: &pb.ClientFrame_Auth{Auth: &pb.AuthFrame{IdToken: "not-a-real-jwt"}}}
	if err := c.Write(ctx, websocket.MessageBinary, mustMarshal(auth)); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read auth result: %v", err)
	}
	var sf pb.ServerFrame
	if err := proto.Unmarshal(data, &sf); err != nil {
		t.Fatalf("unmarshal server frame: %v", err)
	}
	ar := sf.GetAuthResult()
	if ar == nil || ar.Ok {
		t.Fatalf("expected auth result ok=false for invalid token, got %v", sf.Payload)
	}
}
