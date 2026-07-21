package worldsim

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
)

// TestZoneMetadata_RequestReply verifies that worldsim responds to
// worldsim.zones.get with the current zone metadata for all maps.
func TestZoneMetadata_RequestReply(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("worldsim connect: %v", err)
	}
	t.Cleanup(nc.Close)

	// Build a Simulator with two maps and known zones.
	sim := &Simulator{
		World: World{
			zones: map[string]*ZoneRegistry{
				"main": NewZoneRegistry([]*Zone{
					{ID: "wall1", ZoneType: "wall"},
					{ID: "meeting1", ZoneType: "meeting", AvEnabled: true, IsExclusive: true},
					{ID: "portal1", ZoneType: "portal", PortalTargetMap: "second", PortalTargetEntity: "beacon"},
				}, 50, 50),
				"second": NewZoneRegistry([]*Zone{
					{ID: "wall2", ZoneType: "wall"},
					{ID: "av1", ZoneType: "meeting", AvEnabled: true},
				}, 30, 30),
			},
		},
		nc:         nc,
		defaultMap: "main",
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}

	if err := sim.subscribe(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Request zone metadata via NATS request-reply.
	reply, err := nc.Request("worldsim.zones.get", nil, 2*time.Second)
	if err != nil {
		t.Fatalf("request worldsim.zones.get: %v", err)
	}

	var msg zoneMetadataMsg
	if err := json.Unmarshal(reply.Data, &msg); err != nil {
		t.Fatalf("unmarshal zone metadata: %v", err)
	}

	if len(msg.Maps) != 2 {
		t.Fatalf("expected 2 maps, got %d", len(msg.Maps))
	}

	// Check main map zones.
	mainZones := msg.Maps["main"]
	if len(mainZones) != 3 {
		t.Fatalf("expected 3 zones in main, got %d", len(mainZones))
	}
	// Zones are sorted by ID: meeting1, portal1, wall1.
	if mainZones[0].ID != "meeting1" || !mainZones[0].AvEnabled || !mainZones[0].IsExclusive {
		t.Errorf("meeting1 zone mismatch: %+v", mainZones[0])
	}
	if mainZones[1].ID != "portal1" || mainZones[1].PortalTargetMap != "second" || mainZones[1].PortalTargetEntity != "beacon" {
		t.Errorf("portal1 zone mismatch: %+v", mainZones[1])
	}
	if mainZones[2].ID != "wall1" || mainZones[2].ZoneType != "wall" {
		t.Errorf("wall1 zone mismatch: %+v", mainZones[2])
	}

	// Check second map zones.
	secondZones := msg.Maps["second"]
	if len(secondZones) != 2 {
		t.Fatalf("expected 2 zones in second, got %d", len(secondZones))
	}
	// Sorted: av1, wall2.
	if secondZones[0].ID != "av1" || !secondZones[0].AvEnabled {
		t.Errorf("av1 zone mismatch: %+v", secondZones[0])
	}
	if secondZones[1].ID != "wall2" || secondZones[1].ZoneType != "wall" {
		t.Errorf("wall2 zone mismatch: %+v", secondZones[1])
	}
}

// TestZoneMetadata_Broadcast verifies that broadcastZoneMetadata publishes
// the current zone metadata on worldsim.zones.
func TestZoneMetadata_Broadcast(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("worldsim connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			zones: map[string]*ZoneRegistry{
				"main": NewZoneRegistry([]*Zone{
					{ID: "wall1", ZoneType: "wall"},
					{ID: "av1", ZoneType: "meeting", AvEnabled: true},
				}, 50, 50),
			},
		},
		nc:         nc,
		defaultMap: "main",
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}

	gotBroadcast := make(chan []byte, 1)
	nc.Subscribe("worldsim.zones", func(m *nats.Msg) {
		select {
		case gotBroadcast <- m.Data:
		default:
		}
	})
	nc.Flush()

	sim.broadcastZoneMetadata()

	select {
	case data := <-gotBroadcast:
		var msg zoneMetadataMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal broadcast: %v", err)
		}
		zones := msg.Maps["main"]
		if len(zones) != 2 {
			t.Fatalf("expected 2 zones, got %d", len(zones))
		}
		// Sorted: av1, wall1.
		if zones[0].ID != "av1" || !zones[0].AvEnabled {
			t.Errorf("av1 mismatch: %+v", zones[0])
		}
		if zones[1].ID != "wall1" || zones[1].ZoneType != "wall" {
			t.Errorf("wall1 mismatch: %+v", zones[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worldsim.zones broadcast not received")
	}
}
