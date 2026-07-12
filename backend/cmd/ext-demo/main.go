// ext-demo is a minimal first-party extension that registers with the
// worldsim, sends heartbeats, and logs zone enter/exit events.
//
// It demonstrates the extension protocol:
//  1. Publish to extension.demo.register with {extension_id, heartbeat_interval_s}
//  2. Publish to extension.demo.heartbeat every interval
//  3. Subscribe to zone.enter and zone.exit
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string           `json:"extension_id"`
	HeartbeatIntervalS int              `json:"heartbeat_interval_s"`
	OptionsSchema      []optionFieldDef `json:"options_schema,omitempty"`
}

// optionFieldDef declares a single configurable option.
type optionFieldDef struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default"`
}

// demoOptions holds the current option values for ext-demo.
type demoOptions struct {
	LogZoneEvents bool `json:"log_zone_events"`
}

type zoneEvent struct {
	EntityID string `json:"entity_id"`
	ZoneID   string `json:"zone_id"`
	MapID    string `json:"map_id"`
}

func main() {
	startTime := time.Now()
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := envOr("EXTENSION_ID", "demo")
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Initialize OpenTelemetry (opt-in via OTEL_ENABLED). When enabled, the
	// logger is bridged to OTel logs and spans are exported to the configured
	// OTLP endpoint (OpenObserve in production, motel in dev).
	logger, otelShutdown, err := otel.Init(ctx, "ext-"+extID)
	if err != nil {
		logger.Error("otel init", "err", err)
		os.Exit(1)
	}
	defer otelShutdown(context.Background())

	nc, err := nats.Connect(natsURL,
		nats.Name("ext-"+extID),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	var mu sync.Mutex
	opts := demoOptions{LogZoneEvents: true}

	// Subscribe to zone events.
	nc.Subscribe("zone.enter", func(msg *nats.Msg) {
		var ev zoneEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			return
		}
		mu.Lock()
		logIt := opts.LogZoneEvents
		mu.Unlock()
		if logIt {
			logger.Info("zone enter", "entity", ev.EntityID, "zone", ev.ZoneID, "map", ev.MapID)
		}
	})
	nc.Subscribe("zone.exit", func(msg *nats.Msg) {
		var ev zoneEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			return
		}
		mu.Lock()
		logIt := opts.LogZoneEvents
		mu.Unlock()
		if logIt {
			logger.Info("zone exit", "entity", ev.EntityID, "zone", ev.ZoneID, "map", ev.MapID)
		}
	})

	// Subscribe to extension.demo.options for hot-reloadable config.
	nc.Subscribe(fmt.Sprintf("extension.%s.options", extID), func(m *nats.Msg) {
		mu.Lock()
		before := opts
		if err := json.Unmarshal(m.Data, &opts); err != nil {
			logger.Warn("parse options", "err", err)
			opts = before
		} else {
			logger.Info("options updated", "log_zone_events", opts.LogZoneEvents)
		}
		mu.Unlock()
	})

	// Register with the worldsim and re-register periodically (NATS Core
	// pub/sub is fire-and-forget, so the first publish may be lost if the
	// subscriber isn't ready yet).
	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
		OptionsSchema: []optionFieldDef{
			{Name: "log_zone_events", Type: "bool", Default: json.RawMessage("true")},
		},
	})
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	// publishReg sends the registration + heartbeat pair.
	publishReg := func() {
		nc.Publish(regSubject, regData)
		nc.Publish(hbSubject, []byte(extID))
	}

	// worldsim.ready fires when worldsim's subscriptions are live (on startup
	// and on restart). Re-register whenever it fires so we never race the
	// initial publish.
	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		logger.Info("worldsim ready, registering", "map", string(m.Data))
		publishReg()
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})

	// Wait for worldsim.ready before the initial registration. Fall back to
	// registering directly after a timeout (e.g. worldsim was already up and
	// we missed the broadcast on extension restart).
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		logger.Warn("worldsim.ready not received, registering anyway", "id", extID)
		publishReg()
	}
	logger.Info("registered", "id", extID, "heartbeat_s", heartbeatS)

	// Heartbeat + re-register loop.
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	var ticks int
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			// Re-register every 3rd heartbeat (idempotent on worldsim side).
			if ticks%3 == 0 {
				nc.Publish(regSubject, regData)
			}
			publishHealth(nc, "ext-"+extID, startTime)
			ticks++
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func publishHealth(nc *nats.Conn, service string, startTime time.Time) {
	health := map[string]any{
		"service": service,
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(startTime).Round(time.Second).String(),
		"extras":  map[string]any{},
	}
	data, _ := json.Marshal(health)
	nc.Publish("healthz", data)
}
