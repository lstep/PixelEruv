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

	// Register with the worldsim and re-register periodically (NATS Core
	// pub/sub is fire-and-forget, so the first publish may be lost if the
	// subscriber isn't ready yet).
	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        extID,
		HeartbeatIntervalS: heartbeatS,
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

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			// Re-register every 3rd heartbeat (idempotent on worldsim side).
			if time.Now().Unix()%int64(heartbeatS*3) < int64(heartbeatS) {
				nc.Publish(regSubject, regData)
			}
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
