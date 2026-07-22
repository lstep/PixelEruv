// Package main is the audit service. It subscribes to the "audit.event" NATS
// subject, persists events to its own SQLite database (independent of
// worldsim/PocketBase), and serves a Go templates + HTMX web UI.
//
// Storage is behind an EventStore interface. The current implementation uses
// SQLite (modernc.org/sqlite, pure Go, WAL mode). To switch to ClickHouse
// (preferred upgrade — columnar, fast filtered scans, SQL, JSON support) or
// TimescaleDB, implement the EventStore interface with a different driver.
// The SQL queries are standard and port easily.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"

	_ "modernc.org/sqlite"
)

// EventStore is the persistence interface for audit events. Implementations
// must be safe for concurrent use (the NATS subscriber calls Insert from
// multiple goroutines).
//
// To upgrade storage:
//   - ClickHouse: use github.com/ClickHouse/clickhouse-go/v2. Replace the
//     schema with a MergeTree table ordered by (occurred_at, event_type).
//     Queries stay SQL; JSON details map to ClickHouse's String/JSON type.
//     Native TTL replaces manual retention cleanup.
//   - TimescaleDB: use lib/pq or jackc/pgx. Create a hypertable on
//     occurred_at. Retention via drop_chunks policy. JSONB for details.
type EventStore interface {
	Insert(ev audit.Event) error
	Query(filter QueryFilter) ([]StoredEvent, error)
	GetByID(id int64) (StoredEvent, error)
	CountBySeverity(since time.Time) (map[string]int, error)
	CountByType(since time.Time) (map[string]int, error)
	DeleteOlderThan(before time.Time) (int64, error)
	Close() error
}

// QueryFilter controls which events are returned by Query.
type QueryFilter struct {
	EventType string
	Severity  string
	ActorSub  string
	EntityID  string
	Limit     int
	Offset    int
}

// StoredEvent is an audit.Event persisted in the store, with an auto-increment
// ID and the timestamp parsed back to time.Time.
type StoredEvent struct {
	ID        int64
	EventType string
	Severity  string
	Timestamp time.Time
	Actor     audit.Actor
	Details   json.RawMessage
	TraceID   string
}

// SQLiteStore implements EventStore using a local SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) the SQLite database at dbPath and
// initializes the schema. WAL mode is enabled for concurrent read/write.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; WAL allows concurrent readers

	s := &SQLiteStore{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type   TEXT    NOT NULL,
    severity     TEXT    NOT NULL,
    actor_sub    TEXT    NOT NULL DEFAULT '',
    actor_entity TEXT    NOT NULL DEFAULT '',
    actor_client TEXT    NOT NULL DEFAULT '',
    actor_ip     TEXT    NOT NULL DEFAULT '',
    actor_device TEXT    NOT NULL DEFAULT '',
    actor_ext    TEXT    NOT NULL DEFAULT '',
    actor_name   TEXT    NOT NULL DEFAULT '',
    details      TEXT    NOT NULL DEFAULT '',
    trace_id     TEXT    NOT NULL DEFAULT '',
    occurred_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_type      ON audit_events(event_type);
CREATE INDEX IF NOT EXISTS idx_audit_severity   ON audit_events(severity);
CREATE INDEX IF NOT EXISTS idx_audit_actor_sub  ON audit_events(actor_sub);
CREATE INDEX IF NOT EXISTS idx_audit_actor_entity ON audit_events(actor_entity);
CREATE INDEX IF NOT EXISTS idx_audit_occurred   ON audit_events(occurred_at);
`)
	if err != nil {
		return err
	}
	// Migration: add actor_name column for existing databases created before
	// this column existed. SQLite returns "duplicate column name" error if it
	// already exists — safe to ignore.
	_, _ = s.db.Exec("ALTER TABLE audit_events ADD COLUMN actor_name TEXT NOT NULL DEFAULT ''")
	return nil
}

func (s *SQLiteStore) Insert(ev audit.Event) error {
	details, _ := json.Marshal(ev.Details)
	_, err := s.db.Exec(
		`INSERT INTO audit_events
		   (event_type, severity, actor_sub, actor_entity, actor_client,
		    actor_ip, actor_device, actor_ext, actor_name, details, trace_id, occurred_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.EventType, ev.Severity,
		ev.Actor.Sub, ev.Actor.EntityID, ev.Actor.ClientID,
		ev.Actor.IP, ev.Actor.DeviceID, ev.Actor.Extension, ev.Actor.DisplayName,
		string(details), ev.TraceID, ev.Timestamp,
	)
	return err
}

func (s *SQLiteStore) Query(f QueryFilter) ([]StoredEvent, error) {
	q := `SELECT id, event_type, severity, actor_sub, actor_entity, actor_client,
	             actor_ip, actor_device, actor_ext, actor_name, details, trace_id, occurred_at
	      FROM audit_events WHERE 1=1`
	var args []any
	if f.EventType != "" {
		q += " AND event_type = ?"
		args = append(args, f.EventType)
	}
	if f.Severity != "" {
		q += " AND severity = ?"
		args = append(args, f.Severity)
	}
	if f.ActorSub != "" {
		q += " AND actor_sub = ?"
		args = append(args, f.ActorSub)
	}
	if f.EntityID != "" {
		q += " AND actor_entity = ?"
		args = append(args, f.EntityID)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			args = append(args, f.Offset)
		}
	}

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []StoredEvent
	for rows.Next() {
		var se StoredEvent
		var detailsStr string
		var tsStr string
		if err := rows.Scan(&se.ID, &se.EventType, &se.Severity,
			&se.Actor.Sub, &se.Actor.EntityID, &se.Actor.ClientID,
			&se.Actor.IP, &se.Actor.DeviceID, &se.Actor.Extension, &se.Actor.DisplayName,
			&detailsStr, &se.TraceID, &tsStr); err != nil {
			return nil, err
		}
		se.Details = json.RawMessage(detailsStr)
		se.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
		events = append(events, se)
	}
	return events, rows.Err()
}

func (s *SQLiteStore) GetByID(id int64) (StoredEvent, error) {
	var se StoredEvent
	var detailsStr string
	var tsStr string
	err := s.db.QueryRow(
		`SELECT id, event_type, severity, actor_sub, actor_entity, actor_client,
		        actor_ip, actor_device, actor_ext, actor_name, details, trace_id, occurred_at
		 FROM audit_events WHERE id = ?`, id,
	).Scan(&se.ID, &se.EventType, &se.Severity,
		&se.Actor.Sub, &se.Actor.EntityID, &se.Actor.ClientID,
		&se.Actor.IP, &se.Actor.DeviceID, &se.Actor.Extension, &se.Actor.DisplayName,
		&detailsStr, &se.TraceID, &tsStr)
	se.Details = json.RawMessage(detailsStr)
	se.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
	return se, err
}

func (s *SQLiteStore) CountBySeverity(since time.Time) (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT severity, COUNT(*) FROM audit_events WHERE occurred_at >= ? GROUP BY severity`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]int)
	for rows.Next() {
		var sev string
		var count int
		if err := rows.Scan(&sev, &count); err != nil {
			return nil, err
		}
		m[sev] = count
	}
	return m, rows.Err()
}

func (s *SQLiteStore) CountByType(since time.Time) (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT event_type, COUNT(*) FROM audit_events WHERE occurred_at >= ? GROUP BY event_type ORDER BY COUNT(*) DESC`,
		since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[string]int)
	for rows.Next() {
		var et string
		var count int
		if err := rows.Scan(&et, &count); err != nil {
			return nil, err
		}
		m[et] = count
	}
	return m, rows.Err()
}

func (s *SQLiteStore) DeleteOlderThan(before time.Time) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM audit_events WHERE occurred_at < ?`,
		before.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
