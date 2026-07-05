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
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

type registerMsg struct {
	ExtensionID        string `json:"extension_id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
}

type zoneEvent struct {
	EntityID string `json:"entity_id"`
	ZoneID   string `json:"zone_id"`
	MapID    string `json:"map_id"`
}

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	extID := envOr("EXTENSION_ID", "demo")
	heartbeatS := 10

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

	// Subscribe to zone events.
	nc.Subscribe("zone.enter", func(msg *nats.Msg) {
		var ev zoneEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			return
		}
		logger.Info("zone enter", "entity", ev.EntityID, "zone", ev.ZoneID, "map", ev.MapID)
	})
	nc.Subscribe("zone.exit", func(msg *nats.Msg) {
		var ev zoneEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			return
		}
		logger.Info("zone exit", "entity", ev.EntityID, "zone", ev.ZoneID, "map", ev.MapID)
	})

	// Register with the worldsim.
	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
	})
	regSubject := fmt.Sprintf("extension.%s.register", extID)
	if err := nc.Publish(regSubject, regData); err != nil {
		logger.Error("register publish", "err", err)
		os.Exit(1)
	}
	logger.Info("registered", "id", extID, "heartbeat_s", heartbeatS)

	// Heartbeat loop.
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	// Send first heartbeat immediately.
	nc.Publish(hbSubject, []byte(extID))

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
