package worldsim

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestHandleSetAfk verifies that handleSetAfk toggles Entity.AFK, marks
// dirtyName (so the DisplayName component — which carries afk — is
// re-replicated), and is a no-op when the value is unchanged. AFK is not
// persisted to PocketBase and is not broadcast on worldsim.player_status
// (unlike handleSetStatus).
func TestHandleSetAfk(t *testing.T) {
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
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a

	// AFK is false by default (zero value).
	if a.AFK {
		t.Fatal("AFK should default to false")
	}

	// Subscribe to worldsim.player_status to verify AFK does NOT broadcast
	// there (ext-av only cares about DND, not AFK).
	statusSub, err := subNc.SubscribeSync("worldsim.player_status")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// Toggle AFK on.
	sim.handleSetAfk(context.Background(), "c_a", &pb.SetAfkFrame{Afk: true})
	pubNc.Flush()

	if !a.AFK {
		t.Error("entity.AFK = false, want true")
	}
	if !a.dirtyName {
		t.Error("dirtyName not set; afk rides on the DisplayName component")
	}
	if a.AfkSince.IsZero() {
		t.Error("entity.AfkSince = zero, want a timestamp set when AFK turns true")
	}

	// AFK must NOT broadcast on worldsim.player_status.
	if _, err := statusSub.NextMsg(100 * time.Millisecond); err == nil {
		t.Error("AFK should not broadcast on worldsim.player_status")
	}

	// Toggle AFK off — dirtyName set again, manual Status preserved.
	a.dirtyName = false
	a.Status = statusDoNotDisturb // simulate a manual status under the overlay
	sim.handleSetAfk(context.Background(), "c_a", &pb.SetAfkFrame{Afk: false})
	pubNc.Flush()

	if a.AFK {
		t.Error("entity.AFK = true, want false")
	}
	if !a.AfkSince.IsZero() {
		t.Error("entity.AfkSince should reset to zero when AFK clears")
	}
	if !a.dirtyName {
		t.Error("dirtyName not set on AFK clear")
	}
	if a.Status != statusDoNotDisturb {
		t.Errorf("manual Status changed by AFK toggle; got %d, want %d", a.Status, statusDoNotDisturb)
	}

	// No-op: same value (false) should not set dirtyName.
	a.dirtyName = false
	sim.handleSetAfk(context.Background(), "c_a", &pb.SetAfkFrame{Afk: false})
	if a.dirtyName {
		t.Error("no-op AFK toggle (same value) should not set dirtyName")
	}

	// Unknown client is a no-op.
	a.dirtyName = false
	sim.handleSetAfk(context.Background(), "c_unknown", &pb.SetAfkFrame{Afk: true})
	if a.dirtyName {
		t.Error("unknown client should not set dirtyName")
	}
}
