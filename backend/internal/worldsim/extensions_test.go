package worldsim

import (
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
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

// fakeAuditPublisher records audit events published via Emit for assertions.
type fakeAuditPublisher struct {
	mu     sync.Mutex
	events []string // event types
}

func (f *fakeAuditPublisher) Publish(subject string, data []byte) error {
	var ev audit.Event
	if err := json.Unmarshal(data, &ev); err == nil {
		f.mu.Lock()
		f.events = append(f.events, ev.EventType)
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeAuditPublisher) count(eventType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e == eventType {
			n++
		}
	}
	return n
}

// TestExtensionRegister_AuditSuppressedOnReregistration verifies that periodic
// re-registrations (the extension heartbeat mechanism) do not emit
// extension.registered audit events, while a first registration and an interval
// change do. See issue #80.
func TestExtensionRegister_AuditSuppressedOnReregistration(t *testing.T) {
	fake := &fakeAuditPublisher{}
	m := newTestExtensionManager()
	m.nc = fake

	// First registration: emits.
	if err := m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register #1: %v", err)
	}
	if got := fake.count("extension.registered"); got != 1 {
		t.Fatalf("after first registration: got %d extension.registered events, want 1", got)
	}

	// Re-registration with same interval (heartbeat): no emit.
	if err := m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register #2: %v", err)
	}
	if got := fake.count("extension.registered"); got != 1 {
		t.Fatalf("after re-registration: got %d extension.registered events, want 1 (heartbeat suppressed)", got)
	}

	// Re-registration with a changed interval: emits.
	if err := m.Register([]byte(`{"extension_id":"ext-props","heartbeat_interval_s":30}`)); err != nil {
		t.Fatalf("Register #3: %v", err)
	}
	if got := fake.count("extension.registered"); got != 2 {
		t.Fatalf("after interval change: got %d extension.registered events, want 2", got)
	}

	// A different extension's first registration: emits.
	if err := m.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register #4: %v", err)
	}
	if got := fake.count("extension.registered"); got != 3 {
		t.Fatalf("after new extension: got %d extension.registered events, want 3", got)
	}
}
