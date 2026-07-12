package audit

import (
	"encoding/json"
	"testing"
	"time"
)

// fakeConn captures the last published message for inspection.
type fakeConn struct {
	subject string
	data    []byte
}

func (f *fakeConn) Publish(subject string, data []byte) error {
	f.subject = subject
	f.data = data
	return nil
}

func TestEmitPublishesCorrectJSON(t *testing.T) {
	fc := &fakeConn{}

	Emit(fc, "client.connected", SeverityInfo,
		Actor{Sub: "user123", EntityID: "e_abc", ClientID: "c_1", IP: "1.2.3.4"},
		Details{"map": "town", "is_admin": false},
		"trace-abc")

	if fc.subject != Subject {
		t.Fatalf("expected subject %q, got %q", Subject, fc.subject)
	}

	var ev Event
	if err := json.Unmarshal(fc.data, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if ev.EventType != "client.connected" {
		t.Errorf("event_type: got %q, want %q", ev.EventType, "client.connected")
	}
	if ev.Severity != SeverityInfo {
		t.Errorf("severity: got %q, want %q", ev.Severity, SeverityInfo)
	}
	if ev.Actor.Sub != "user123" {
		t.Errorf("actor.sub: got %q, want %q", ev.Actor.Sub, "user123")
	}
	if ev.Actor.EntityID != "e_abc" {
		t.Errorf("actor.entity_id: got %q, want %q", ev.Actor.EntityID, "e_abc")
	}
	if ev.Details["map"] != "town" {
		t.Errorf("details.map: got %v, want %q", ev.Details["map"], "town")
	}
	if ev.TraceID != "trace-abc" {
		t.Errorf("trace_id: got %q, want %q", ev.TraceID, "trace-abc")
	}
	// Timestamp should be valid RFC3339.
	if _, err := time.Parse(time.RFC3339, ev.Timestamp); err != nil {
		t.Errorf("timestamp not RFC3339: %v", err)
	}
}

func TestEmitOmitsEmptyFields(t *testing.T) {
	fc := &fakeConn{}
	Emit(fc, "auth.failed", SeverityWarn,
		Actor{IP: "5.6.7.8"},
		nil,
		"")

	var ev Event
	if err := json.Unmarshal(fc.data, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if ev.Actor.Sub != "" {
		t.Errorf("expected empty actor.sub, got %q", ev.Actor.Sub)
	}
	if ev.TraceID != "" {
		t.Errorf("expected empty trace_id, got %q", ev.TraceID)
	}
	// Details and trace_id should be omitted from JSON (omitempty).
	var raw map[string]json.RawMessage
	json.Unmarshal(fc.data, &raw)
	if _, ok := raw["details"]; ok {
		t.Error("expected details to be omitted from JSON")
	}
	if _, ok := raw["trace_id"]; ok {
		t.Error("expected trace_id to be omitted from JSON")
	}
}
