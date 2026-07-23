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
	"sort"
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
	ListPlayers() ([]PlayerSummary, error)
	PlayerSessions(sub string) ([]Session, error)
	PlayerEvents(sub string, limit int) ([]StoredEvent, error)
	PlayerActivityEvents(sub string, since time.Time) ([]StoredEvent, error)
	Close() error
}

// PlayerSummary is one row in the /audit/players leaderboard. It aggregates
// per-actor_sub data for logged-in players (guests with empty sub and the
// local "dev" fallback are excluded).
type PlayerSummary struct {
	Sub            string
	DisplayName    string
	FirstSeen      time.Time
	LastSeen       time.Time
	EventCount     int
	ConnectCount   int
	TotalSessionNs int64 // sum of session durations; open sessions count up to now
	Created        time.Time // from PocketBase players.created (registration date)
	IsAdmin        bool
}

// Session is one connect→disconnect pairing for a player. DisconnectedAt is
// the zero time when the session is still open (no disconnect event recorded,
// e.g. server crash or still connected).
type Session struct {
	ClientID      string
	ConnectedAt   time.Time
	DisconnectedAt time.Time
	Duration      time.Duration // DisconnectedAt - ConnectedAt, or now - ConnectedAt if open
	Open          bool
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

// sessionRow is the raw shape of the connect/disconnect pairing query.
type sessionRow struct {
	Sub          string
	ClientID     string
	ConnectedAt  string
	DisconnectedAt sql.NullString
}

// sessionPairingQuery returns connect/disconnect pairs grouped by
// (actor_sub, actor_client). Each client_id is unique per WebSocket session
// (see pusher.generateClientID), so each group has at most one connect and
// one disconnect. Groups without a connect are filtered out. If filterSub is
// non-empty, only that player's sessions are returned.
func (s *SQLiteStore) sessionPairingQuery(filterSub string) ([]sessionRow, error) {
	q := `SELECT actor_sub, actor_client,
	             MIN(CASE WHEN event_type='client.connected' THEN occurred_at END) AS connected_at,
	             MIN(CASE WHEN event_type='client.disconnected' THEN occurred_at END) AS disconnected_at
	      FROM audit_events
	      WHERE event_type IN ('client.connected', 'client.disconnected')
	        AND actor_sub NOT IN ('', 'dev')`
	var args []any
	if filterSub != "" {
		q += " AND actor_sub = ?"
		args = append(args, filterSub)
	}
	q += " GROUP BY actor_sub, actor_client HAVING connected_at IS NOT NULL ORDER BY connected_at DESC"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.Sub, &r.ClientID, &r.ConnectedAt, &r.DisconnectedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// sessionFromRow converts a sessionRow into a Session, computing the
// duration. Open sessions (no disconnect) are left with a zero
// DisconnectedAt and Duration=0; capOpenSessions fills those in.
func sessionFromRow(r sessionRow, now time.Time) (Session, error) {
	connected, err := time.Parse(time.RFC3339, r.ConnectedAt)
	if err != nil {
		return Session{}, fmt.Errorf("parse connected_at: %w", err)
	}
	sess := Session{
		ClientID:    r.ClientID,
		ConnectedAt: connected,
	}
	if r.DisconnectedAt.Valid && r.DisconnectedAt.String != "" {
		disc, err := time.Parse(time.RFC3339, r.DisconnectedAt.String)
		if err != nil {
			return Session{}, fmt.Errorf("parse disconnected_at: %w", err)
		}
		sess.DisconnectedAt = disc
		sess.Duration = disc.Sub(connected)
	} else {
		sess.Open = true
	}
	return sess, nil
}

// capOpenSessions fixes the duration of open sessions (no disconnect event).
// An open session is capped at the player's next connect time — if the player
// reconnected with a new client_id, the old WebSocket was dropped, so the
// session ended when the new one started. The most recent open session per
// player counts up to now. This prevents orphaned connects (server crash,
// lost disconnect event) from inflating total session time to absurd values.
func capOpenSessions(sessions []Session, now time.Time) {
	// Sort by connected_at ascending to find "next connect" per session.
	// sessions comes from sessionPairingQuery ordered DESC, so reverse.
	type sorted struct {
		idx  int
		conn time.Time
	}
	sortedSessions := make([]sorted, len(sessions))
	for i, s := range sessions {
		sortedSessions[i] = sorted{i, s.ConnectedAt}
	}
	sort.Slice(sortedSessions, func(i, j int) bool {
		return sortedSessions[i].conn.Before(sortedSessions[j].conn)
	})

	for i, ss := range sortedSessions {
		sess := &sessions[ss.idx]
		if !sess.Open {
			continue
		}
		// Find the next connect for the same player (any client_id).
		var end time.Time
		if i+1 < len(sortedSessions) {
			end = sortedSessions[i+1].conn
		} else {
			// Most recent session — count up to now.
			end = now
		}
		if end.Before(sess.ConnectedAt) {
			end = sess.ConnectedAt
		}
		sess.DisconnectedAt = end
		sess.Duration = end.Sub(sess.ConnectedAt)
	}
}

func (s *SQLiteStore) ListPlayers() ([]PlayerSummary, error) {
	// Per-sub aggregates: latest non-empty display name, first/last seen,
	// event count, connect count.
	q := `SELECT actor_sub,
	             MAX(CASE WHEN actor_name != '' THEN actor_name ELSE NULL END) AS display_name,
	             MIN(occurred_at) AS first_seen,
	             MAX(occurred_at) AS last_seen,
	             COUNT(*) AS event_count,
	             SUM(CASE WHEN event_type='client.connected' THEN 1 ELSE 0 END) AS connect_count
	      FROM audit_events
	      WHERE actor_sub NOT IN ('', 'dev')
	      GROUP BY actor_sub
	      ORDER BY last_seen DESC`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type aggRow struct {
		Sub           string
		DisplayName   sql.NullString
		FirstSeen     string
		LastSeen      string
		EventCount    int
		ConnectCount  int
	}
	aggs := make(map[string]aggRow)
	var subs []string
	for rows.Next() {
		var a aggRow
		if err := rows.Scan(&a.Sub, &a.DisplayName, &a.FirstSeen, &a.LastSeen, &a.EventCount, &a.ConnectCount); err != nil {
			return nil, err
		}
		aggs[a.Sub] = a
		subs = append(subs, a.Sub)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Session durations per sub. Build sessions, cap open sessions at the
	// next connect for the same player, then sum.
	now := time.Now().UTC()
	pairs, err := s.sessionPairingQuery("")
	if err != nil {
		return nil, err
	}
	sessionsBySub := make(map[string][]Session)
	for _, r := range pairs {
		sess, err := sessionFromRow(r, now)
		if err != nil {
			return nil, err
		}
		sessionsBySub[r.Sub] = append(sessionsBySub[r.Sub], sess)
	}
	totalNs := make(map[string]int64)
	for sub, sessions := range sessionsBySub {
		capOpenSessions(sessions, now)
		for _, sess := range sessions {
			totalNs[sub] += int64(sess.Duration)
		}
	}

	out := make([]PlayerSummary, 0, len(subs))
	for _, sub := range subs {
		a := aggs[sub]
		firstSeen, _ := time.Parse(time.RFC3339, a.FirstSeen)
		lastSeen, _ := time.Parse(time.RFC3339, a.LastSeen)
		name := ""
		if a.DisplayName.Valid {
			name = a.DisplayName.String
		}
		out = append(out, PlayerSummary{
			Sub:            sub,
			DisplayName:    name,
			FirstSeen:      firstSeen,
			LastSeen:       lastSeen,
			EventCount:     a.EventCount,
			ConnectCount:   a.ConnectCount,
			TotalSessionNs: totalNs[sub],
		})
	}
	// Sort by total session time desc.
	sort.Slice(out, func(i, j int) bool {
		return out[i].TotalSessionNs > out[j].TotalSessionNs
	})
	return out, nil
}

func (s *SQLiteStore) PlayerSessions(sub string) ([]Session, error) {
	pairs, err := s.sessionPairingQuery(sub)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]Session, 0, len(pairs))
	for _, r := range pairs {
		sess, err := sessionFromRow(r, now)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	capOpenSessions(out, now)
	return out, nil
}

func (s *SQLiteStore) PlayerEvents(sub string, limit int) ([]StoredEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	// Events where actor_sub = ? OR actor_entity belongs to this player.
	// player.set_status / player.set_afk carry entity_id but not sub, so the
	// entity join is required to capture them.
	q := `SELECT id, event_type, severity, actor_sub, actor_entity, actor_client,
	             actor_ip, actor_device, actor_ext, actor_name, details, trace_id, occurred_at
	      FROM audit_events
	      WHERE actor_sub = ?
	         OR actor_entity IN (SELECT DISTINCT actor_entity FROM audit_events
	                              WHERE actor_sub = ? AND actor_entity != '')
	      ORDER BY id DESC
	      LIMIT ?`
	rows, err := s.db.Query(q, sub, sub, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStoredEvents(rows)
}

func (s *SQLiteStore) PlayerActivityEvents(sub string, since time.Time) ([]StoredEvent, error) {
	q := `SELECT id, event_type, severity, actor_sub, actor_entity, actor_client,
	             actor_ip, actor_device, actor_ext, actor_name, details, trace_id, occurred_at
	      FROM audit_events
	      WHERE occurred_at >= ?
	        AND event_type IN ('client.connected', 'client.disconnected', 'player.set_status', 'player.set_afk')
	        AND (actor_sub = ?
	             OR actor_entity IN (SELECT DISTINCT actor_entity FROM audit_events
	                                  WHERE actor_sub = ? AND actor_entity != ''))
	      ORDER BY occurred_at ASC`
	rows, err := s.db.Query(q, since.UTC().Format(time.RFC3339), sub, sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStoredEvents(rows)
}

// scanStoredEvents scans a standard 13-column StoredEvent result set into a
// slice. Shared by Query, PlayerEvents, and PlayerActivityEvents.
func scanStoredEvents(rows *sql.Rows) ([]StoredEvent, error) {
	var events []StoredEvent
	for rows.Next() {
		var se StoredEvent
		var detailsStr, tsStr string
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
