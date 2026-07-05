package worldsim

import (
	"log/slog"
	"testing"
	"time"
)

func newTestExtensionManager() *ExtensionManager {
	return NewExtensionManager(slog.Default())
}

func TestExtensionManager_InputTriggerRegistration(t *testing.T) {
	m := newTestExtensionManager()

	if err := m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := m.RegisterTriggers([]byte(`{
		"extension_id": "ext-props",
		"input_triggers": [{"input": "key:E"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	exts := m.ExtensionsForInput("key:E")
	if len(exts) != 1 || exts[0] != "ext-props" {
		t.Fatalf("ExtensionsForInput(key:E) = %v, want [ext-props]", exts)
	}

	if got := m.ExtensionsForInput("key:F"); len(got) != 0 {
		t.Errorf("ExtensionsForInput(key:F) = %v, want empty", got)
	}
}

func TestExtensionManager_InputTriggerCoexistence(t *testing.T) {
	// Two extensions can register for the same input type (generic +
	// dedicated), matching the design's "both" ownership model.
	m := newTestExtensionManager()
	m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":10}`))
	m.Register([]byte(`{"extension_id":"ext-vault-door","heartbeat_interval_s":10}`))
	m.RegisterTriggers([]byte(`{"extension_id":"ext-props","input_triggers":[{"input":"key:E"}]}`))
	m.RegisterTriggers([]byte(`{"extension_id":"ext-vault-door","input_triggers":[{"input":"key:E"}]}`))

	exts := m.ExtensionsForInput("key:E")
	if len(exts) != 2 {
		t.Fatalf("expected both extensions registered for key:E, got %v", exts)
	}
}

func TestExtensionManager_StaleExtensionExcludedFromInput(t *testing.T) {
	m := newTestExtensionManager()
	m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":1}`))
	m.RegisterTriggers([]byte(`{"extension_id":"ext-props","input_triggers":[{"input":"key:E"}]}`))

	// Force staleness by rewinding the last heartbeat.
	m.mu.Lock()
	m.extensions["ext-props"].LastHeartbeat = time.Now().Add(-10 * time.Second)
	m.mu.Unlock()

	if got := m.ExtensionsForInput("key:E"); len(got) != 0 {
		t.Errorf("expected stale extension excluded, got %v", got)
	}
}
