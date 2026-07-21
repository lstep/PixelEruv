// Package extkit provides shared lifecycle helpers for extension mains
// (ext-demo, ext-walls, ext-props, ext-av, ext-rec). Each main calls
// these helpers inline rather than duplicating boilerplate.
//
// This is a helper library, not a framework: each main keeps full control
// of its lifecycle. See documentation/plans/2026-07-22-extkit-design.md.
package extkit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

// EnvOr returns the env var value or fallback if unset/empty.
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ConnectNATS connects to NATS with the standard extension options
// (Name "ext-<extID>", 2s reconnect, infinite retries).
func ConnectNATS(url, extID string) (*nats.Conn, error) {
	return nats.Connect(url,
		nats.Name("ext-"+extID),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
}

// PublishHealth publishes a health JSON to the "healthz" NATS subject.
// service is typically "ext-<extID>".
func PublishHealth(nc *nats.Conn, service string, startTime time.Time) {
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

// RegisterMsg is the registration payload published to
// extension.<id>.register.
type RegisterMsg struct {
	ExtensionID        string           `json:"extension_id"`
	HeartbeatIntervalS int              `json:"heartbeat_interval_s"`
	OptionsSchema      []OptionFieldDef `json:"options_schema,omitempty"`
}

// OptionFieldDef declares a single configurable option for an extension.
type OptionFieldDef struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default"`
}

// WaitForReady subscribes to "worldsim.ready", calls onReady with the map
// name from the broadcast (or "" on timeout), and returns. onReady performs
// extension-specific init + registration in both cases. The subscription
// stays live after the function returns so later worldsim restarts also
// trigger onReady.
func WaitForReady(nc *nats.Conn, logger *slog.Logger, timeout time.Duration, onReady func(mapName string)) {
	readyCh := make(chan struct{}, 1)
	nc.Subscribe("worldsim.ready", func(m *nats.Msg) {
		mapName := string(m.Data)
		logger.Info("worldsim ready, registering", "map", mapName)
		onReady(mapName)
		select {
		case readyCh <- struct{}{}:
		default:
		}
	})
	select {
	case <-readyCh:
	case <-time.After(timeout):
		logger.Warn("worldsim.ready not received, registering anyway")
		onReady("")
	}
}

// HeartbeatLoop runs the heartbeat + re-register + health-publish loop
// until ctx is cancelled. Publishes a heartbeat every heartbeatS seconds,
// calls onReRegister every 3rd tick, and publishes health every tick.
func HeartbeatLoop(ctx context.Context, nc *nats.Conn, extID string, heartbeatS int, onReRegister func()) {
	hbSubject := fmt.Sprintf("extension.%s.heartbeat", extID)
	ticker := time.NewTicker(time.Duration(heartbeatS) * time.Second)
	defer ticker.Stop()

	startTime := time.Now()
	var ticks int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nc.Publish(hbSubject, []byte(extID))
			if ticks%3 == 0 {
				onReRegister()
			}
			PublishHealth(nc, "ext-"+extID, startTime)
			ticks++
		}
	}
}

// SubscribeOptions subscribes to extension.<extID>.options for hot-reload.
// opts must be a pointer to the extension's options struct. On parse error
// the previous value is restored (via JSON snapshot/restore) and a warning
// is logged. On success onReload is called (may be nil). The mutex protects
// opts access across the hot-reload callback and the extension's own
// goroutines.
func SubscribeOptions(nc *nats.Conn, extID string, opts any, mu *sync.Mutex, logger *slog.Logger, onReload func()) error {
	_, err := nc.Subscribe(fmt.Sprintf("extension.%s.options", extID), func(m *nats.Msg) {
		mu.Lock()
		defer mu.Unlock()
		// Snapshot current state so we can roll back on parse error.
		// json.Marshal on a pointer marshals the pointed-to struct;
		// json.Unmarshal back into the pointer restores it.
		snapshot, _ := json.Marshal(opts)
		if err := json.Unmarshal(m.Data, opts); err != nil {
			logger.Warn("parse options", "err", err)
			json.Unmarshal(snapshot, opts)
			return
		}
		if onReload != nil {
			onReload()
		}
	})
	return err
}
