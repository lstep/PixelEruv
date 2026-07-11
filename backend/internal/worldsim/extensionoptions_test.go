package worldsim

import (
	"encoding/json"
	"log/slog"
	"testing"
)

func TestBuildDefaultsJSON(t *testing.T) {
	schema := []optionFieldDef{
		{Name: "enabled", Type: "bool", Default: json.RawMessage("true")},
		{Name: "radius", Type: "number", Default: json.RawMessage("1.5")},
		{Name: "label", Type: "text", Default: json.RawMessage(`"hello"`)},
	}
	result := buildDefaultsJSON(schema)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}

	if len(m) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(m))
	}

	var b bool
	if err := json.Unmarshal(m["enabled"], &b); err != nil || !b {
		t.Errorf("enabled = %v, want true", b)
	}

	var f float64
	if err := json.Unmarshal(m["radius"], &f); err != nil || f != 1.5 {
		t.Errorf("radius = %v, want 1.5", f)
	}

	var s string
	if err := json.Unmarshal(m["label"], &s); err != nil || s != "hello" {
		t.Errorf("label = %q, want hello", s)
	}
}

func TestBuildDefaultsJSON_TypeFallbacks(t *testing.T) {
	// When no default is provided, the type determines the fallback.
	schema := []optionFieldDef{
		{Name: "flag", Type: "bool"},
		{Name: "count", Type: "number"},
		{Name: "name", Type: "text"},
	}
	result := buildDefaultsJSON(schema)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}

	var b bool
	if err := json.Unmarshal(m["flag"], &b); err != nil || b {
		t.Errorf("flag fallback = %v, want false", b)
	}

	var f float64
	if err := json.Unmarshal(m["count"], &f); err != nil || f != 0 {
		t.Errorf("count fallback = %v, want 0", f)
	}

	var s string
	if err := json.Unmarshal(m["name"], &s); err != nil || s != "" {
		t.Errorf("name fallback = %q, want empty", s)
	}
}

func TestExtensionManager_RegisterWithOptionsSchema(t *testing.T) {
	m := newTestExtensionManager()

	// Register with an options schema. Without a PB app, the options
	// manager is nil, so EnsureOptions/PublishOptions are skipped — but
	// the registration itself should still succeed.
	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        "test-ext",
		HeartbeatIntervalS: 10,
		OptionsSchema: []optionFieldDef{
			{Name: "enabled", Type: "bool", Default: json.RawMessage("true")},
		},
	})
	if err := m.Register(regData); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !m.IsRegistered("test-ext") {
		t.Fatal("extension not registered")
	}
}

func TestExtensionManager_RegisterWithoutOptionsSchema(t *testing.T) {
	m := newTestExtensionManager()

	regData, _ := json.Marshal(registerMsg{
		ExtensionID:        "test-ext",
		HeartbeatIntervalS: 10,
	})
	if err := m.Register(regData); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !m.IsRegistered("test-ext") {
		t.Fatal("extension not registered")
	}
}

// TestExtensionOptionsManager_NilApp ensures that EnsureOptions and
// PublishOptions gracefully handle a nil app (test mode without PB).
func TestExtensionOptionsManager_NilApp(t *testing.T) {
	mgr := NewExtensionOptionsManager(nil, nil, slog.Default())

	// EnsureOptions should return an error, not panic.
	_, _, err := mgr.EnsureOptions("test", nil)
	if err == nil {
		t.Error("expected error with nil app")
	}

	// PublishOptions should be a no-op, not panic.
	mgr.PublishOptions("test")
	mgr.PublishAllOptions()
}
