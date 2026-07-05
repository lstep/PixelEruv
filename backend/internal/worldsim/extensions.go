package worldsim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Extension is a registered extension process on the NATS bus.
type Extension struct {
	ID              string
	HeartbeatInterval time.Duration
	LastHeartbeat   time.Time
}

// ExtensionManager handles extension registration, heartbeats, and lifecycle.
type ExtensionManager struct {
	mu         sync.Mutex
	extensions map[string]*Extension
	logger     *slog.Logger
}

func NewExtensionManager(logger *slog.Logger) *ExtensionManager {
	return &ExtensionManager{
		extensions: make(map[string]*Extension),
		logger:     logger,
	}
}

// Register handles an extension registration message.
type registerMsg struct {
	ExtensionID        string `json:"extension_id"`
	HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
}

func (m *ExtensionManager) Register(data []byte) error {
	var msg registerMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("parse register: %w", err)
	}
	if msg.ExtensionID == "" {
		return fmt.Errorf("missing extension_id")
	}
	interval := time.Duration(msg.HeartbeatIntervalS) * time.Second
	if interval == 0 {
		interval = 10 * time.Second
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.extensions[msg.ExtensionID] = &Extension{
		ID:               msg.ExtensionID,
		HeartbeatInterval: interval,
		LastHeartbeat:    time.Now(),
	}
	m.logger.Info("extension registered", "id", msg.ExtensionID, "heartbeat", interval)
	return nil
}

// Heartbeat updates the last-seen time for an extension.
func (m *ExtensionManager) Heartbeat(extensionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ext, ok := m.extensions[extensionID]; ok {
		ext.LastHeartbeat = time.Now()
	}
}

// CheckStale freezes extensions that have missed 3x their heartbeat interval.
// Returns the IDs of newly-stale extensions.
func (m *ExtensionManager) CheckStale() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var stale []string
	now := time.Now()
	for id, ext := range m.extensions {
		if now.Sub(ext.LastHeartbeat) > 3*ext.HeartbeatInterval {
			stale = append(stale, id)
			m.logger.Warn("extension stale, freezing", "id", id,
				"last_heartbeat", ext.LastHeartbeat,
				"missed", now.Sub(ext.LastHeartbeat))
		}
	}
	return stale
}

// IsRegistered returns true if the extension is known and not stale.
func (m *ExtensionManager) IsRegistered(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ext, ok := m.extensions[id]
	if !ok {
		return false
	}
	return time.Since(ext.LastHeartbeat) <= 3*ext.HeartbeatInterval
}

// Subscribe sets up NATS subscriptions for extension lifecycle events.
func (m *ExtensionManager) Subscribe(nc *nats.Conn) error {
	// Wildcard subscription: extension.<id>.register
	if _, err := nc.Subscribe("extension.*.register", func(msg *nats.Msg) {
		if err := m.Register(msg.Data); err != nil {
			m.logger.Warn("extension register failed", "err", err, "subject", msg.Subject)
		}
	}); err != nil {
		return fmt.Errorf("subscribe register: %w", err)
	}

	// Wildcard subscription: extension.<id>.heartbeat
	if _, err := nc.Subscribe("extension.*.heartbeat", func(msg *nats.Msg) {
		extID := extractExtensionID(msg.Subject, "heartbeat")
		m.Heartbeat(extID)
	}); err != nil {
		return fmt.Errorf("subscribe heartbeat: %w", err)
	}

	return nil
}

// extractExtensionID gets the extension ID from a subject like
// "extension.walls.heartbeat" -> "walls".
func extractExtensionID(subject, suffix string) string {
	prefix := "extension."
	s := subject[len(prefix):]
	end := len(s) - len("."+suffix)
	if end < 0 {
		return ""
	}
	return s[:end]
}

// StartStaleChecker runs a periodic check for stale extensions.
func (m *ExtensionManager) StartStaleChecker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckStale()
		}
	}
}
