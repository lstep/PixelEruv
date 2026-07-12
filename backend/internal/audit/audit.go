// Package audit provides a lightweight event-emission helper for the audit
// log system. Emitters (pusher, worldsim, extensions) call Emit to publish a
// structured audit event to the NATS subject "audit.event". A standalone
// service (backend/cmd/audit) subscribes, persists events to its own SQLite
// database, and serves a web UI for searching and browsing.
//
// Audit events capture lifecycle + interaction granularity: who connected,
// who was banned, chat messages, name/sprite changes, zone transitions, etc.
// They are NOT per-frame input or replication — those stay in OTel traces.
//
// Each event carries an optional trace_id that links to the corresponding
// OpenTelemetry trace in OpenObserve, so the audit UI can deep-link to the
// full trace for debugging.
package audit

import (
	"encoding/json"
	"time"
)

// Subject is the NATS subject all audit events are published to.
const Subject = "audit.event"

// Publisher is the minimal interface Emit needs from a NATS connection.
// *nats.Conn satisfies this.
type Publisher interface {
	Publish(subject string, data []byte) error
}

// Severity levels for audit events.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Actor describes who or what triggered the event. All fields are optional;
// fill what's known at the call site.
type Actor struct {
	Sub       string `json:"sub,omitempty"`        // OIDC subject (logged-in users)
	EntityID  string `json:"entity_id,omitempty"`  // World entity ID
	ClientID  string `json:"client_id,omitempty"`  // Pusher session ID
	IP        string `json:"ip,omitempty"`         // Client IP
	DeviceID  string `json:"device_id,omitempty"`  // Browser device ID
	Extension string `json:"extension,omitempty"`  // Extension ID (for ext events)
}

// Details holds event-specific key/value pairs serialized as JSON.
type Details map[string]any

// Event is the audit event envelope published to NATS and persisted by the
// audit service.
type Event struct {
	EventType string   `json:"event_type"`
	Severity  string   `json:"severity"`
	Timestamp string   `json:"timestamp"` // RFC3339
	Actor     Actor    `json:"actor"`
	Details   Details  `json:"details,omitempty"`
	TraceID   string   `json:"trace_id,omitempty"`
}

// Emit publishes an audit event to NATS. It never blocks on errors — if the
// publish fails, the event is silently dropped (audit is best-effort, not a
// critical path). The timestamp is set automatically.
func Emit(nc Publisher, eventType, severity string, actor Actor, details Details, traceID string) {
	ev := Event{
		EventType: eventType,
		Severity:  severity,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Actor:     actor,
		Details:   details,
		TraceID:   traceID,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = nc.Publish(Subject, data)
}
