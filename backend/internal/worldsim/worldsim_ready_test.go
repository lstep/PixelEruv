package worldsim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
)

// startEmbeddedNATS runs an in-process NATS server on a free port for tests.
func startEmbeddedNATS(t *testing.T) (*server.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)
	opts := test.DefaultTestOptions
	opts.Host = host
	opts.Port, _ = net.LookupPort("tcp", port)
	srv, err := server.NewServer(&opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv, fmt.Sprintf("nats://%s", addr)
}

// TestWorldsimReady_PublishedAfterSubscribe is a regression test for the
// ~30s startup delay. worldsim must publish worldsim.ready after its
// subscriptions are live (with Flush), so that an extension subscribing
// before worldsim starts will receive the broadcast and register immediately
// — without waiting for the periodic re-register cycle.
func TestWorldsimReady_PublishedAfterSubscribe(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("worldsim connect: %v", err)
	}
	t.Cleanup(nc.Close)

	// Build a minimal Simulator with only the fields subscribe() touches at
	// setup time (nc, extMgr, defaultMap, logger, tracer). This avoids the
	// LoadMap PocketBase retry (30s) that New() would trigger.
	sim := &Simulator{
		nc:         nc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}

	// Simulate an extension: subscribe to worldsim.ready BEFORE worldsim
	// calls subscribe(), so the broadcast is guaranteed to be delivered.
	extNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("extension connect: %v", err)
	}
	t.Cleanup(extNc.Close)

	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        "test-ext",
		HeartbeatIntervalS: 10,
	})
	gotReady := make(chan string, 1)
	extNc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		extNc.Publish("extension.test-ext.register", regData)
		select {
		case gotReady <- string(m.Data):
		default:
		}
	})
	extNc.Flush()

	// Now worldsim subscribes — this publishes worldsim.ready + Flush.
	if err := sim.subscribe(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// The extension should receive worldsim.ready and register immediately.
	select {
	case mapID := <-gotReady:
		if mapID != "test-map" {
			t.Errorf("worldsim.ready payload = %q, want test-map", mapID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worldsim.ready not received by extension subscriber")
	}

	// Wait for the extension's registration publish to be processed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sim.extMgr.IsRegistered("test-ext") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sim.extMgr.IsRegistered("test-ext") {
		t.Fatal("extension not registered after worldsim.ready broadcast; " +
			"the worldsim.ready signal may not have been published or flushed")
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
