package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
)

// newTestStore creates an in-memory SQLite store for testing.
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	store, err := NewSQLiteStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// insertEvent is a test helper that inserts an audit event with the given fields.
func insertEvent(t *testing.T, store *SQLiteStore, eventType, sub, entityID, clientID, name string, ts time.Time) {
	t.Helper()
	err := store.Insert(audit.Event{
		EventType: eventType,
		Severity:  audit.SeverityInfo,
		Timestamp: ts.UTC().Format(time.RFC3339),
		Actor: audit.Actor{
			Sub:         sub,
			EntityID:    entityID,
			ClientID:    clientID,
			DisplayName: name,
		},
	})
	if err != nil {
		t.Fatalf("Insert %s: %v", eventType, err)
	}
}

func TestListPlayers(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)

	// Player 1: two sessions (1h closed + 30m open).
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_a", "Alice", base)
	insertEvent(t, store, "client.disconnected", "sub1", "e1", "c_a", "Alice", base.Add(1*time.Hour))
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_b", "Alice", base.Add(2*time.Hour))
	// No disconnect for c_b — open session.

	// Player 2: one closed session (2h).
	insertEvent(t, store, "client.connected", "sub2", "e2", "c_c", "Bob", base)
	insertEvent(t, store, "client.disconnected", "sub2", "e2", "c_c", "Bob", base.Add(2*time.Hour))

	// Guest (empty sub) — should be excluded.
	insertEvent(t, store, "client.connected", "", "e3", "c_d", "Guest", base)

	// Dev — should be excluded.
	insertEvent(t, store, "client.connected", "dev", "e4", "c_e", "Dev", base)

	players, err := store.ListPlayers()
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if len(players) != 2 {
		t.Fatalf("expected 2 players, got %d: %+v", len(players), players)
	}

	// Alice should be first (1h closed + open session > Bob's 2h, since the
	// open session counts from base+2h to now).
	var alice, bob *PlayerSummary
	for i := range players {
		switch players[i].Sub {
		case "sub1":
			alice = &players[i]
		case "sub2":
			bob = &players[i]
		}
	}
	if alice == nil || bob == nil {
		t.Fatalf("missing players: %+v", players)
	}
	if alice.DisplayName != "Alice" {
		t.Errorf("Alice name = %q", alice.DisplayName)
	}
	if alice.ConnectCount != 2 {
		t.Errorf("Alice connect count = %d, want 2", alice.ConnectCount)
	}
	if bob.ConnectCount != 1 {
		t.Errorf("Bob connect count = %d, want 1", bob.ConnectCount)
	}
	// Bob's total session time = 2h.
	bobDur := time.Duration(bob.TotalSessionNs)
	if bobDur != 2*time.Hour {
		t.Errorf("Bob total session = %v, want 2h", bobDur)
	}
	// Alice's total session time = 1h + (now - base+2h). The open session
	// duration is variable, so just check it's > 1h.
	aliceDur := time.Duration(alice.TotalSessionNs)
	if aliceDur <= 1*time.Hour {
		t.Errorf("Alice total session = %v, want > 1h (1h closed + open session)", aliceDur)
	}
	// Alice should be ranked first (more total time than Bob).
	if players[0].Sub != "sub1" {
		t.Errorf("expected Alice first, got %s", players[0].Sub)
	}
}

func TestPlayerSessions(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)

	// Two sessions: one closed, one open.
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_a", "Alice", base)
	insertEvent(t, store, "client.disconnected", "sub1", "e1", "c_a", "Alice", base.Add(1*time.Hour))
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_b", "Alice", base.Add(2*time.Hour))

	sessions, err := store.PlayerSessions("sub1")
	if err != nil {
		t.Fatalf("PlayerSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	// Ordered by connected_at DESC — c_b first.
	if sessions[0].ClientID != "c_b" {
		t.Errorf("first session client = %s, want c_b", sessions[0].ClientID)
	}
	if !sessions[0].Open {
		t.Errorf("first session should be open")
	}
	if sessions[1].ClientID != "c_a" {
		t.Errorf("second session client = %s, want c_a", sessions[1].ClientID)
	}
	if sessions[1].Open {
		t.Errorf("second session should be closed")
	}
	if sessions[1].Duration != 1*time.Hour {
		t.Errorf("second session duration = %v, want 1h", sessions[1].Duration)
	}
}

func TestPlayerEvents(t *testing.T) {
	store := newTestStore(t)
	base := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)

	// Connect event with sub.
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_a", "Alice", base)
	// Status event with entity_id but NO sub — should be matched via entity join.
	err := store.Insert(audit.Event{
		EventType: "player.set_status",
		Severity:  audit.SeverityInfo,
		Timestamp: base.Add(1 * time.Hour).UTC().Format(time.RFC3339),
		Actor: audit.Actor{
			EntityID:    "e1",
			ClientID:    "c_a",
			DisplayName: "Alice",
		},
		Details: audit.Details{"status": 1},
	})
	if err != nil {
		t.Fatalf("Insert set_status: %v", err)
	}
	// Event from a different player — should NOT be included.
	insertEvent(t, store, "client.connected", "sub2", "e2", "c_b", "Bob", base)

	events, err := store.PlayerEvents("sub1", 100)
	if err != nil {
		t.Fatalf("PlayerEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (connect + set_status), got %d", len(events))
	}
	// Ordered by id DESC — set_status (id=2) first.
	if events[0].EventType != "player.set_status" {
		t.Errorf("first event = %s, want player.set_status", events[0].EventType)
	}
	if events[1].EventType != "client.connected" {
		t.Errorf("second event = %s, want client.connected", events[1].EventType)
	}
}

func TestPlayerActivityEvents(t *testing.T) {
	store := newTestStore(t)
	base := time.Now().UTC().Add(-1 * time.Hour)

	// Activity events (should be included).
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_a", "Alice", base)
	insertEvent(t, store, "player.set_status", "sub1", "e1", "c_a", "Alice", base.Add(10*time.Minute))
	insertEvent(t, store, "player.set_afk", "sub1", "e1", "c_a", "Alice", base.Add(20*time.Minute))
	insertEvent(t, store, "client.disconnected", "sub1", "e1", "c_a", "Alice", base.Add(30*time.Minute))

	// Non-activity event (should be excluded).
	insertEvent(t, store, "chat.message", "sub1", "e1", "c_a", "Alice", base.Add(15*time.Minute))

	// Old event outside the time window (should be excluded).
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_old", "Alice", base.Add(-8*24*time.Hour))

	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	events, err := store.PlayerActivityEvents("sub1", since)
	if err != nil {
		t.Fatalf("PlayerActivityEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 activity events, got %d: %+v", len(events), events)
	}
	// Ordered by occurred_at ASC.
	if events[0].EventType != "client.connected" {
		t.Errorf("first = %s, want client.connected", events[0].EventType)
	}
	if events[3].EventType != "client.disconnected" {
		t.Errorf("last = %s, want client.disconnected", events[3].EventType)
	}
	// Verify no chat.message or old events.
	for _, ev := range events {
		if ev.EventType == "chat.message" {
			t.Errorf("chat.message should not be in activity events")
		}
		if ev.Actor.ClientID == "c_old" {
			t.Errorf("old event should not be in activity events")
		}
	}
}

func TestCapOpenSessions(t *testing.T) {
	store := newTestStore(t)
	// Use recent timestamps so the final open session's "now" delta is small.
	base := time.Now().UTC().Add(-2 * time.Hour)

	// Simulate the Luc bug: many orphaned connects (no disconnect) spread over
	// 2 hours, then a final open session. Without capping, each orphaned connect
	// counts up to now, inflating total time to ~2h * N orphans.
	for i := 0; i < 10; i++ {
		cid := fmt.Sprintf("c_%d", i)
		insertEvent(t, store, "client.connected", "sub1", "e1", cid, "Alice", base.Add(time.Duration(i)*6*time.Minute))
		// No disconnect — orphaned.
	}
	// Final session (also open — the player is currently connected).
	insertEvent(t, store, "client.connected", "sub1", "e1", "c_final", "Alice", base.Add(1*time.Hour))

	players, err := store.ListPlayers()
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if len(players) != 1 {
		t.Fatalf("expected 1 player, got %d", len(players))
	}

	// With capping: each orphaned session i is capped at the next connect
	// (base + (i+1)*6m), so each contributes 6m = 10*6m = 60m.
	// The final session counts from base+1h to now (~1h).
	// Total ~ 2h. Without capping: 10 orphans * ~2h + 1h = ~21h (the bug).
	dur := time.Duration(players[0].TotalSessionNs)
	if dur > 3*time.Hour {
		t.Errorf("total session = %v, want <= 3h (capping should prevent inflation)", dur)
	}
	if dur < 1*time.Hour {
		t.Errorf("total session = %v, want >= 1h (10 capped sessions + final open session)", dur)
	}

	// Also verify via PlayerSessions.
	sessions, err := store.PlayerSessions("sub1")
	if err != nil {
		t.Fatalf("PlayerSessions: %v", err)
	}
	var total time.Duration
	for _, s := range sessions {
		total += s.Duration
	}
	if total > 3*time.Hour {
		t.Errorf("PlayerSessions total = %v, want <= 3h", total)
	}
}
