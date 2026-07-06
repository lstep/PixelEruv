package worldsim

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
)

// TestZoneEvent_ContainsClientID verifies that zone.enter/zone.exit events
// include the player's client_id in the NATS payload (empty for base
// entities without a NetworkSession). ext-av needs client_id to address
// LiveKit token replies to the correct client.
func TestZoneEvent_ContainsClientID(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Use a separate connection for subscribing, to avoid same-connection
	// delivery quirks.
	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		nc:     pubNc,
		mapID:  "test-map",
		logger: logger,
		tracer: otel.Tracer("test"),
	}

	type zoneEventPayload struct {
		EntityID string `json:"entity_id"`
		ClientID string `json:"client_id"`
		ZoneID   string `json:"zone_id"`
		MapID    string `json:"map_id"`
	}

	sub, err := subNc.SubscribeSync("zone.enter")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// Publish a zone.enter for a player (with client_id) and a base entity
	// (without client_id).
	sim.publishZoneEvent(context.Background(), "zone.enter", "e_player", "c_abc", "z1")
	sim.publishZoneEvent(context.Background(), "zone.enter", "e_base", "", "z1")
	pubNc.Flush()

	got := map[string]zoneEventPayload{}
	for len(got) < 2 {
		msg, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("expected 2 events, got %d: %v", len(got), err)
		}
		var ev zoneEventPayload
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got[ev.EntityID] = ev
	}

	playerEv := got["e_player"]
	if playerEv.ClientID != "c_abc" {
		t.Errorf("player zone.enter client_id = %q, want %q", playerEv.ClientID, "c_abc")
	}
	if playerEv.ZoneID != "z1" {
		t.Errorf("player zone.enter zone_id = %q, want z1", playerEv.ZoneID)
	}

	baseEv := got["e_base"]
	if baseEv.ClientID != "" {
		t.Errorf("base entity zone.enter client_id = %q, want empty", baseEv.ClientID)
	}
}
