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

	"github.com/lstep/pixeleruv/backend/internal/extkit"
	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/nats-io/nats.go"
)

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
	natsURL := extkit.EnvOr("NATS_URL", "nats://localhost:4222")
	extID := extkit.EnvOr("EXTENSION_ID", "demo")
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

	nc, err := extkit.ConnectNATS(natsURL, extID)
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
	if err := extkit.SubscribeOptions(nc, extID, &opts, &mu, logger, func() {
		logger.Info("options updated", "log_zone_events", opts.LogZoneEvents)
	}); err != nil {
		logger.Error("subscribe options", "err", err)
		os.Exit(1)
	}

	// Register with the worldsim and re-register periodically (NATS Core
	// pub/sub is fire-and-forget, so the first publish may be lost if the
	// subscriber isn't ready yet).
	regData, _ := json.Marshal(extkit.RegisterMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
		OptionsSchema: []extkit.OptionFieldDef{
			{Name: "log_zone_events", Type: "bool", Default: json.RawMessage("true")},
		},
	})
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)

	// publishReg sends the registration + heartbeat pair (used for initial
	// registration on worldsim.ready). The heartbeat loop's re-register
	// callback only publishes the registration since HeartbeatLoop already
	// publishes heartbeats.
	publishReg := func() {
		nc.Publish(regSubject, regData)
		nc.Publish(hbSubject, []byte(extID))
	}

	extkit.WaitForReady(nc, logger, 10*time.Second, func(_ string) {
		publishReg()
	})
	logger.Info("registered", "id", extID, "heartbeat_s", heartbeatS)

	// Heartbeat + re-register loop. onReRegister publishes only the
	// registration (HeartbeatLoop handles the heartbeat + health publish).
	extkit.HeartbeatLoop(ctx, nc, extID, heartbeatS, func() {
		nc.Publish(regSubject, regData)
	})
	logger.Info("shutting down")
}
