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

// GateBehavior determines whether movement into a zone is allowed.
type GateBehavior int

const (
	GateBlock  GateBehavior = iota // block movement
	GateAllow                      // allow movement
)

// GateTrigger is a zone gate trigger registered by an extension.
// The worldsim caches these locally and evaluates them during movement
// without a NATS round-trip.
type GateTrigger struct {
	ZoneID      string
	Behavior    GateBehavior
	ExtensionID string
}

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
	gateTriggers map[string]*GateTrigger // zone_id -> trigger
	// inputTriggers maps an input type (e.g. "key:E") to the set of
	// extension IDs registered for it. See 14-zones-and-interactions.md §3a.
	inputTriggers map[string]map[string]bool
	logger     *slog.Logger
}

func NewExtensionManager(logger *slog.Logger) *ExtensionManager {
	return &ExtensionManager{
		extensions:    make(map[string]*Extension),
		gateTriggers:  make(map[string]*GateTrigger),
		inputTriggers: make(map[string]map[string]bool),
		logger:        logger,
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

// triggerMsg is the payload for extension.<id>.register_triggers.
type triggerMsg struct {
	ExtensionID string `json:"extension_id"`
	GateTriggers []struct {
		ZoneID   string `json:"zone_id"`
		Behavior string `json:"behavior"` // "block" or "allow"
	} `json:"gate_triggers"`
	// InputTriggers registers the extension for player-initiated input
	// events (key presses, clicks) — see 14-zones-and-interactions.md §3a.
	// Unlike gate triggers, these are not bound to a zone; the kernel
	// broadcasts every matching input event to all registered extensions,
	// which self-filter based on the dispatched payload (adjacent entities).
	InputTriggers []struct {
		Input string `json:"input"` // e.g. "key:E", "click:left"
	} `json:"input_triggers"`
}

// RegisterTriggers processes a trigger registration message from an extension.
func (m *ExtensionManager) RegisterTriggers(data []byte) error {
	var msg triggerMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("parse triggers: %w", err)
	}
	if msg.ExtensionID == "" {
		return fmt.Errorf("missing extension_id")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, gt := range msg.GateTriggers {
		if gt.ZoneID == "" {
			continue
		}
		var behavior GateBehavior
		switch gt.Behavior {
		case "block":
			behavior = GateBlock
		case "allow":
			behavior = GateAllow
		default:
			m.logger.Warn("unknown gate behavior", "behavior", gt.Behavior, "zone", gt.ZoneID)
			continue
		}
		m.gateTriggers[gt.ZoneID] = &GateTrigger{
			ZoneID:      gt.ZoneID,
			Behavior:    behavior,
			ExtensionID: msg.ExtensionID,
		}
		m.logger.Info("gate trigger registered",
			"extension", msg.ExtensionID, "zone", gt.ZoneID, "behavior", gt.Behavior)
	}
	for _, it := range msg.InputTriggers {
		if it.Input == "" {
			continue
		}
		if m.inputTriggers[it.Input] == nil {
			m.inputTriggers[it.Input] = make(map[string]bool)
		}
		m.inputTriggers[it.Input][msg.ExtensionID] = true
		m.logger.Info("input trigger registered",
			"extension", msg.ExtensionID, "input", it.Input)
	}
	return nil
}

// ExtensionsForInput returns the IDs of active (non-stale) extensions
// registered for the given input type (e.g. "key:E").
func (m *ExtensionManager) ExtensionsForInput(input string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []string
	for extID := range m.inputTriggers[input] {
		ext, ok := m.extensions[extID]
		if !ok || time.Since(ext.LastHeartbeat) > 3*ext.HeartbeatInterval {
			continue
		}
		result = append(result, extID)
	}
	return result
}

// IsZoneBlocked returns true if the zone has a block gate trigger from a
// registered (non-stale) extension.
func (m *ExtensionManager) IsZoneBlocked(zoneID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	gt, ok := m.gateTriggers[zoneID]
	if !ok {
		return false
	}
	// Only honor triggers from active extensions.
	ext, exists := m.extensions[gt.ExtensionID]
	if !exists {
		return false
	}
	if time.Since(ext.LastHeartbeat) > 3*ext.HeartbeatInterval {
		return false // stale extension — don't block
	}
	return gt.Behavior == GateBlock
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

	// Wildcard subscription: extension.<id>.register_triggers
	if _, err := nc.Subscribe("extension.*.register_triggers", func(msg *nats.Msg) {
		if err := m.RegisterTriggers(msg.Data); err != nil {
			m.logger.Warn("trigger registration failed", "err", err, "subject", msg.Subject)
		}
	}); err != nil {
		return fmt.Errorf("subscribe register_triggers: %w", err)
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
