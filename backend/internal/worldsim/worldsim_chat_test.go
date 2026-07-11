package worldsim

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// newChatTestSim builds a minimal Simulator wired to an embedded NATS,
// ready for handleChat tests. Returns the sim and a subscriber connection.
func newChatTestSim(t *testing.T) (*Simulator, *nats.Conn) {
	t.Helper()
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	sim := &Simulator{
		nc:              pubNc,
		defaultMap:      "test-map",
		zones:           map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
		extMgr:          NewExtensionManager(logger),
		logger:          logger,
		tracer:          otel.Tracer("test"),
		entities:        map[string]*Entity{},
		clients:         map[string]*Entity{},
		entityIDToClient: map[string]string{},
	}
	return sim, subNc
}

// addPlayer registers a player entity on the sim with the given display name
// and proximity group.
func addPlayer(sim *Simulator, entityID, clientID, displayName, group string) *Entity {
	e := &Entity{
		ID:                    entityID,
		Position:              &pb.Position{X: 5, Y: 5},
		NetworkSession:        &NetworkSession{ClientID: clientID, Input: &pb.InputState{}},
		DisplayName:           displayName,
		SpriteBase:            "",
		currentZones:          make(map[string]bool),
		spawnedTo:             make(map[string]bool),
		currentProximityGroup: group,
	}
	sim.entities[entityID] = e
	sim.clients[clientID] = e
	sim.entityIDToClient[entityID] = clientID
	return e
}

// decodeChatMessage unmarshals a ServerFrame from NATS and returns the
// ChatMessageFrame inside it, failing the test if it's not present.
func decodeChatMessage(t *testing.T, data []byte) *pb.ChatMessageFrame {
	t.Helper()
	var sf pb.ServerFrame
	if err := proto.Unmarshal(data, &sf); err != nil {
		t.Fatalf("unmarshal ServerFrame: %v", err)
	}
	cm := sf.GetChatMessage()
	if cm == nil {
		t.Fatalf("ServerFrame payload is not ChatMessage: %T", sf.Payload)
	}
	return cm
}

// TestChat_GlobalBroadcast verifies that a global chat message is published
// to chat.broadcast with the sender's stamped display name and a truncated
// text body.
func TestChat_GlobalBroadcast(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "")

	broadcastSub, err := subNc.SubscribeSync("chat.broadcast")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	sim.handleChat(t.Context(), "c_alice", &pb.ChatFrame{
		Channel: "global",
		Text:    "hello world",
	})
	sim.nc.Flush()

	msg, err := broadcastSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no chat.broadcast message: %v", err)
	}
	cm := decodeChatMessage(t, msg.Data)
	if cm.Channel != "global" {
		t.Errorf("channel = %q, want global", cm.Channel)
	}
	if cm.EntityId != "e_alice" {
		t.Errorf("entity_id = %q, want e_alice", cm.EntityId)
	}
	if cm.DisplayName != "Alice" {
		t.Errorf("display_name = %q, want Alice", cm.DisplayName)
	}
	if cm.Text != "hello world" {
		t.Errorf("text = %q, want hello world", cm.Text)
	}
	if cm.Timestamp == 0 {
		t.Error("timestamp not set")
	}
}

// TestChat_TruncatesLongText verifies that text over 500 runes is truncated
// to exactly 500 runes.
func TestChat_TruncatesLongText(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "")

	broadcastSub, err := subNc.SubscribeSync("chat.broadcast")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	long := strings.Repeat("x", 600)
	sim.handleChat(t.Context(), "c_alice", &pb.ChatFrame{
		Channel: "global",
		Text:    long,
	})
	sim.nc.Flush()

	msg, err := broadcastSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no message: %v", err)
	}
	cm := decodeChatMessage(t, msg.Data)
	if got := len([]rune(cm.Text)); got != maxChatRunes {
		t.Errorf("text rune count = %d, want %d", got, maxChatRunes)
	}
}

// TestChat_TruncatesOnRuneBoundary verifies that truncation splits on a
// rune boundary, not a byte boundary, so multi-byte characters aren't
// corrupted.
func TestChat_TruncatesOnRuneBoundary(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "")

	broadcastSub, err := subNc.SubscribeSync("chat.broadcast")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// 600 repetitions of "é" (2 bytes each in UTF-8). Truncating at byte
	// 500 would split a rune; truncating at rune 500 must not.
	long := strings.Repeat("é", 600)
	sim.handleChat(t.Context(), "c_alice", &pb.ChatFrame{
		Channel: "global",
		Text:    long,
	})
	sim.nc.Flush()

	msg, err := broadcastSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no message: %v", err)
	}
	cm := decodeChatMessage(t, msg.Data)
	if got := len([]rune(cm.Text)); got != maxChatRunes {
		t.Errorf("text rune count = %d, want %d", got, maxChatRunes)
	}
	if strings.ContainsRune(cm.Text, '\uFFFD') {
		t.Error("truncated text contains replacement rune — byte-split truncation")
	}
}

// TestChat_ProximityDelivery verifies that a proximity chat message is
// delivered to each member of the sender's proximity group (including the
// sender echo), each on their own client.<id>.chat_inbox subject.
func TestChat_ProximityDelivery(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	// A and B share a group; C is in a different group; D is solo.
	addPlayer(sim, "e_alice", "c_alice", "Alice", "proxgroup-1")
	addPlayer(sim, "e_bob", "c_bob", "Bob", "proxgroup-1")
	addPlayer(sim, "e_carol", "c_carol", "Carol", "proxgroup-2")
	addPlayer(sim, "e_dave", "c_dave", "Dave", "")

	aInbox, _ := subNc.SubscribeSync("client.c_alice.chat_inbox")
	bInbox, _ := subNc.SubscribeSync("client.c_bob.chat_inbox")
	cInbox, _ := subNc.SubscribeSync("client.c_carol.chat_inbox")
	dInbox, _ := subNc.SubscribeSync("client.c_dave.chat_inbox")
	broadcastSub, _ := subNc.SubscribeSync("chat.broadcast")
	subNc.Flush()

	sim.handleChat(t.Context(), "c_alice", &pb.ChatFrame{
		Channel: "proximity",
		Text:    "hi neighbors",
	})
	sim.nc.Flush()

	// A and B should each receive one message.
	for _, sub := range []*nats.Subscription{aInbox, bInbox} {
		msg, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("expected message on inbox, got: %v", err)
		}
		cm := decodeChatMessage(t, msg.Data)
		if cm.Channel != "proximity" {
			t.Errorf("channel = %q, want proximity", cm.Channel)
		}
		if cm.EntityId != "e_alice" {
			t.Errorf("entity_id = %q, want e_alice", cm.EntityId)
		}
		if cm.Text != "hi neighbors" {
			t.Errorf("text = %q, want hi neighbors", cm.Text)
		}
	}

	// Carol and Dave should NOT receive anything.
	if _, err := cInbox.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("Carol received proximity message from a different group")
	}
	if _, err := dInbox.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("Dave (solo) received proximity message")
	}
	// Proximity must not leak to the global broadcast subject.
	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("proximity chat leaked to chat.broadcast")
	}
}

// TestChat_ProximitySoloDropped verifies that a proximity message from a
// player with no current group is dropped silently (no publishes at all).
func TestChat_ProximitySoloDropped(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_dave", "c_dave", "Dave", "")

	inbox, _ := subNc.SubscribeSync("client.c_dave.chat_inbox")
	broadcastSub, _ := subNc.SubscribeSync("chat.broadcast")
	subNc.Flush()

	sim.handleChat(t.Context(), "c_dave", &pb.ChatFrame{
		Channel: "proximity",
		Text:    "anyone there?",
	})
	sim.nc.Flush()

	if _, err := inbox.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("solo proximity sender received an echo")
	}
	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("solo proximity message leaked to chat.broadcast")
	}
}

// TestChat_GuestDisplayName verifies that a guest entity (sub == "") gets
// a "Guest <last4>" display name stamped on outgoing messages.
func TestChat_GuestDisplayName(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	// provisionClient computes DisplayName for guests; call it to exercise
	// the real path. It needs a userStore == nil (no PocketBase in tests)
	// so the guest branch is taken.
	sim.userStore = nil
	entityID, _ := sim.provisionClient(t.Context(), "c_abc12345", "")
	if entityID == "" {
		t.Fatal("provisionClient returned empty entity id")
	}
	e := sim.clients["c_abc12345"]
	want := "Guest " + lastN(entityID, 4)
	if e.DisplayName != want {
		t.Fatalf("provisioned DisplayName = %q, want %q", e.DisplayName, want)
	}

	broadcastSub, _ := subNc.SubscribeSync("chat.broadcast")
	subNc.Flush()

	sim.handleChat(t.Context(), "c_abc12345", &pb.ChatFrame{
		Channel: "global",
		Text:    "hi",
	})
	sim.nc.Flush()

	msg, err := broadcastSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no message: %v", err)
	}
	cm := decodeChatMessage(t, msg.Data)
	if cm.DisplayName != want {
		t.Errorf("display_name = %q, want %q", cm.DisplayName, want)
	}
}

// TestChat_UnknownClientDropped verifies that a chat frame from a client
// not in s.clients is dropped silently (no panic, no publishes).
func TestChat_UnknownClientDropped(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	broadcastSub, _ := subNc.SubscribeSync("chat.broadcast")
	subNc.Flush()

	// Should not panic.
	sim.handleChat(t.Context(), "c_bogus", &pb.ChatFrame{
		Channel: "global",
		Text:    "ghost",
	})
	sim.nc.Flush()

	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("unknown client produced a broadcast")
	}
}

// TestChat_UnknownChannelDropped verifies that an unrecognized channel is
// dropped silently.
func TestChat_UnknownChannelDropped(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "proxgroup-1")

	broadcastSub, _ := subNc.SubscribeSync("chat.broadcast")
	inbox, _ := subNc.SubscribeSync("client.c_alice.chat_inbox")
	subNc.Flush()

	sim.handleChat(t.Context(), "c_alice", &pb.ChatFrame{
		Channel: "telepathy",
		Text:    "can you hear me",
	})
	sim.nc.Flush()

	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("unknown channel produced a broadcast")
	}
	if _, err := inbox.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("unknown channel produced an inbox message")
	}
}
