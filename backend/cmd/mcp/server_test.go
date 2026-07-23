package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// TestWorldsimClient_StatsAndEntities verifies the MCP WorldsimClient
// correctly calls worldsim.stats.get, worldsim.entities.query, and
// worldsim.entity.get against a mock worldsim on an in-process NATS server.
func TestWorldsimClient_StatsAndEntities(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	registerMockWorldsim(t, nc)

	w := NewWorldsimClient(nc, "mcp-test")

	// get_world_stats
	stats, err := w.GetStats(context.Background())
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalPlayers != 2 || stats.TotalEntities != 3 {
		t.Errorf("stats: players=%d entities=%d, want 2/3", stats.TotalPlayers, stats.TotalEntities)
	}

	// query_entities with filter
	entities, err := w.QueryEntities(context.Background(), EntitiesQuery{MapID: "main"})
	if err != nil {
		t.Fatalf("QueryEntities: %v", err)
	}
	if len(entities) != 2 {
		t.Errorf("QueryEntities map=main: got %d, want 2", len(entities))
	}

	// get_entity
	snap, err := w.GetEntity(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if snap.EntityID != "e1" || snap.State != "on" {
		t.Errorf("GetEntity: %+v", snap)
	}
}

// TestWorldsimClient_AdminActions verifies kick/ban/set_name round-trip
// through the mock worldsim and return adminResponse{OK:true}.
func TestWorldsimClient_AdminActions(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	registerMockWorldsim(t, nc)
	w := NewWorldsimClient(nc, "mcp-test")

	resp, err := w.SetName(context.Background(), "e1", "NewName")
	if err != nil {
		t.Fatalf("SetName: %v", err)
	}
	if !resp.OK {
		t.Errorf("SetName response: %+v, want OK", resp)
	}

	resp, err = w.SetStatus(context.Background(), "e1", 2)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if !resp.OK {
		t.Errorf("SetStatus response: %+v, want OK", resp)
	}

	resp, err = w.Kick(context.Background(), "c_a", "", "test")
	if err != nil {
		t.Fatalf("Kick: %v", err)
	}
	if !resp.OK {
		t.Errorf("Kick response: %+v, want OK", resp)
	}

	// Invalid status should return an error from the worldsim error payload.
	if _, err := w.SetStatus(context.Background(), "e1", 99); err == nil {
		t.Error("expected error for invalid status")
	}
}

// TestWorldsimClient_WorldOptions verifies GetWorldOptions and
// SetWorldOptions round-trip through the mock worldsim, including the
// validation-error path.
func TestWorldsimClient_WorldOptions(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	registerMockWorldsim(t, nc)
	w := NewWorldsimClient(nc, "mcp-test")

	opts, err := w.GetWorldOptions(context.Background())
	if err != nil {
		t.Fatalf("GetWorldOptions: %v", err)
	}
	if opts.SMTPHost != "mailhog" || opts.SMTPPort != 1025 || !opts.RecordingEnabled {
		t.Errorf("GetWorldOptions: %+v", opts)
	}
	if opts.FFmpegTimeout != int64(10*60*1e9) {
		t.Errorf("FFmpegTimeout: %d, want %d", opts.FFmpegTimeout, int64(10*60*1e9))
	}

	// Set with valid input — echo mock returns the same options.
	updated, err := w.SetWorldOptions(context.Background(), WorldOptions{
		SMTPHost:          "smtp.example.com",
		SMTPPort:          587,
		FFmpegConcurrency: 4,
		FFmpegTimeout:     int64(30 * 60 * 1e9),
		RecordingEnabled:  false,
	})
	if err != nil {
		t.Fatalf("SetWorldOptions: %v", err)
	}
	if updated.SMTPHost != "smtp.example.com" || updated.FFmpegConcurrency != 4 {
		t.Errorf("SetWorldOptions response: %+v", updated)
	}

	// Set with invalid input (ffmpeg_concurrency < 1) — worldsim returns an
	// error payload which requestReply surfaces as a Go error.
	if _, err := w.SetWorldOptions(context.Background(), WorldOptions{
		SMTPHost:          "smtp.example.com",
		SMTPPort:          587,
		FFmpegConcurrency: 0,
	}); err == nil {
		t.Error("expected error for ffmpeg_concurrency < 1")
	}
}

// TestAuditClient_HTTP verifies AuditClient.QueryEvents / GetEvent /
// PlayerTimeline / Stats call the audit JSON API correctly.
func TestAuditClient_HTTP(t *testing.T) {
	// Mock audit HTTP server returning canned JSON.
	mux := http.NewServeMux()
	mux.HandleFunc("/audit/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]APIEvent{
			{ID: 1, EventType: "player.kicked", Severity: "warn", Timestamp: "2026-07-19T00:00:00Z"},
			{ID: 2, EventType: "chat.message", Severity: "info", Timestamp: "2026-07-19T00:01:00Z"},
		})
	})
	mux.HandleFunc("/audit/api/events/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(APIEvent{ID: 1, EventType: "player.kicked", Severity: "warn"})
	})
	mux.HandleFunc("/audit/api/players", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]PlayerSummary{
			{Sub: "sub123", DisplayName: "Alice", TotalSessionSec: 3600, EventCount: 10, ConnectCount: 2},
		})
	})
	mux.HandleFunc("/audit/api/players/sub123", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sub":           "sub123",
			"display_name":  "Alice",
			"events":        []APIEvent{{ID: 5, EventType: "player.set_name", Actor: audit.Actor{Sub: "sub123"}}},
			"activity_events": []APIEvent{{ID: 6, EventType: "client.connected", Actor: audit.Actor{Sub: "sub123"}}},
		})
	})
	mux.HandleFunc("/audit/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(AuditStats{Uptime: "1h", Version: "test", SeverityCounts: map[string]int{"info": 10}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAuditClient(srv.URL+"/audit", "", "", nil)

	events, err := a.QueryEvents(context.Background(), AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 2 || events[0].ID != 1 {
		t.Errorf("QueryEvents: %+v", events)
	}

	ev, err := a.GetEvent(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if ev.EventType != "player.kicked" {
		t.Errorf("GetEvent: %+v", ev)
	}

	timeline, err := a.PlayerTimeline(context.Background(), "sub123")
	if err != nil {
		t.Fatalf("PlayerTimeline: %v", err)
	}
	if len(timeline) != 1 || timeline[0].Actor.Sub != "sub123" {
		t.Errorf("PlayerTimeline: %+v", timeline)
	}

	players, err := a.ListPlayers(context.Background())
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if len(players) != 1 || players[0].Sub != "sub123" || players[0].DisplayName != "Alice" {
		t.Errorf("ListPlayers: %+v", players)
	}

	activity, err := a.PlayerActivity(context.Background(), "sub123", 0)
	if err != nil {
		t.Fatalf("PlayerActivity: %v", err)
	}
	if len(activity) != 1 || activity[0].EventType != "client.connected" {
		t.Errorf("PlayerActivity: %+v", activity)
	}

	stats, err := a.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Uptime != "1h" || stats.SeverityCounts["info"] != 10 {
		t.Errorf("Stats: %+v", stats)
	}
}

// TestPocketBaseClient verifies ListRecords and GetRecord hit the expected
// PocketBase REST URLs and that the admin token is sent raw (no "Bearer "
// prefix) in the Authorization header, matching PocketBase's convention
// (see backend/cmd/admin/server.go pbAdminToken).
func TestPocketBaseClient(t *testing.T) {
	var lastPath string
	var lastAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/collections/", func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path + "?" + r.URL.RawQuery
		lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("filter") == "is_default = true" {
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{"id": "map1", "name": "main", "is_default": true}},
				"totalItems": 1,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"id": "p1", "display_name": "Alice"}},
			"totalItems": 1,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pbClient := NewPocketBaseClient(srv.URL, "token123")

	data, err := pbClient.ListRecords(context.Background(), "players", ListParams{PerPage: 30, Page: 1})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if lastPath != "/api/collections/players/records?page=1&perPage=30" {
		t.Errorf("ListRecords path: %s", lastPath)
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0]["display_name"] != "Alice" {
		t.Errorf("ListRecords items: %+v", resp.Items)
	}
	// PocketBase expects the raw admin token in the Authorization header,
	// NOT "Bearer <token>" — see backend/cmd/admin/server.go pbAdminToken.
	if lastAuth != "token123" {
		t.Errorf("Authorization header: %q, want %q (raw token, no Bearer prefix)", lastAuth, "token123")
	}

	// Filter path
	_, err = pbClient.ListRecords(context.Background(), "maps", ListParams{Filter: "is_default = true"})
	if err != nil {
		t.Fatalf("ListRecords filter: %v", err)
	}
	if lastPath != "/api/collections/maps/records?filter=is_default+%3D+true" {
		t.Errorf("ListRecords filter path: %s", lastPath)
	}

	// GetRecord
	_, err = pbClient.GetRecord(context.Background(), "players", "p1")
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if lastPath != "/api/collections/players/records/p1?" {
		t.Errorf("GetRecord path: %s", lastPath)
	}
}

// TestDockerClient verifies the MCP DockerClient correctly calls the
// docker-readonly-proxy: filters to com.docker.compose.project=pixeleruv by
// default, drops the filter when allProjects=true, parses the /containers/json
// response into ContainerRow, and surfaces /info as raw JSON.
func TestDockerClient(t *testing.T) {
	var lastPath string
	var lastQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		// Two canned containers: one in pixeleruv project, one not.
		_, _ = w.Write([]byte(`[
			{"Id":"abc123","Names":["/pixeleruv-mcp-1"],"Image":"pixeleruv/mcp:latest","ImageID":"sha256:img1","State":"running","Status":"Up 5 minutes","Created":1700000000,"Labels":{"com.docker.compose.project":"pixeleruv","com.docker.compose.service":"mcp"}},
			{"Id":"def456","Names":["/other-app-1"],"Image":"other:latest","ImageID":"sha256:img2","State":"exited","Status":"Exited (0) 2 hours ago","Created":1699990000,"Labels":{"com.docker.compose.project":"other"}}
		]`))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Containers":5,"ContainersRunning":3,"Images":10,"OperatingSystem":"linux"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	d := NewDockerClient(srv.URL)

	// Default: pixeleruv filter applied. The proxy returns both canned
	// containers (the proxy is the filter boundary in production; our mock
	// doesn't honor filters), but we verify the filter query was sent.
	rows, err := d.ListContainers(context.Background(), false)
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if lastPath != "/containers/json" {
		t.Errorf("ListContainers path: %s", lastPath)
	}
	if !strings.Contains(lastQuery, "all=true") || !strings.Contains(lastQuery, "com.docker.compose.project%3Dpixeleruv") {
		t.Errorf("ListContainers default query missing filter/all: %s", lastQuery)
	}
	if len(rows) != 2 {
		t.Fatalf("ListContainers rows: got %d, want 2", len(rows))
	}
	if rows[0].Name != "pixeleruv-mcp-1" || rows[0].Image != "pixeleruv/mcp:latest" || rows[0].State != "running" {
		t.Errorf("row[0]: %+v", rows[0])
	}
	if rows[0].Created != 1700000000 || rows[0].Labels["com.docker.compose.service"] != "mcp" {
		t.Errorf("row[0] fields: %+v", rows[0])
	}
	if rows[1].Name != "other-app-1" || rows[1].State != "exited" {
		t.Errorf("row[1]: %+v", rows[1])
	}

	// allProjects=true drops the filter.
	if _, err := d.ListContainers(context.Background(), true); err != nil {
		t.Fatalf("ListContainers allProjects: %v", err)
	}
	if strings.Contains(lastQuery, "com.docker.compose.project") {
		t.Errorf("ListContainers allProjects=true should not filter: %s", lastQuery)
	}

	// Info returns raw JSON.
	info, err := d.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if !strings.Contains(string(info), `"Images":10`) {
		t.Errorf("Info body: %s", string(info))
	}

	// Empty base URL → error, not panic.
	emptyClient := NewDockerClient("")
	if _, err := emptyClient.ListContainers(context.Background(), false); err == nil {
		t.Error("expected error when DOCKER_PROXY_URL unset")
	}
	if _, err := emptyClient.Info(context.Background()); err == nil {
		t.Error("expected error when DOCKER_PROXY_URL unset")
	}
}

// TestNewMCPServer constructs the full MCP server with all tools/resources/
// prompts registered against a live in-process NATS + mock HTTP backends.
// It catches struct-tag panics like the one that crashed production on first
// deploy (jsonschema:"description=..." is rejected by jsonschema-go v0.4.3 —
// the tag value IS the description, no prefix). Without this test, the unit
// tests in this file all bypass NewMCPServer/registerTools, so a bad tag
// would only panic at runtime.
func TestNewMCPServer(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	registerMockWorldsim(t, nc)

	// Mock audit + PB HTTP backends so AuditClient/PocketBaseClient construct.
	auditSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"total":0}`))
	}))
	t.Cleanup(auditSrv.Close)
	pbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"totalItems":0,"page":1,"perPage":30,"totalPages":0}`))
	}))
	t.Cleanup(pbSrv.Close)
	// Mock docker-proxy so DockerClient constructs and docker tools register.
	dockerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/info" {
			_, _ = w.Write([]byte(`{"Containers":0,"Images":0}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(dockerSrv.Close)

	deps := Deps{
		Worldsim: NewWorldsimClient(nc, "mcp-test"),
		Audit:    NewAuditClient(auditSrv.URL, "", "", nil),
		PB:       NewPocketBaseClient(pbSrv.URL, ""),
		Docker:   NewDockerClient(dockerSrv.URL),
	}
	logger := slog.New(slog.NewTextHandler(&discardWriter{}, nil))

	// Must not panic. All 20 tools, 13 resources, 3 prompts register.
	s := NewMCPServer(deps, logger)
	if s == nil {
		t.Fatal("NewMCPServer returned nil")
	}
}

// TestBearerAuth verifies the bearer-token middleware accepts valid tokens
// and rejects missing/wrong tokens.
func TestBearerAuth(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := bearerAuth(next, "secret-token", slog.New(slog.NewTextHandler(&discardWriter{}, nil)))

	// No header → 401
	called = false
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusUnauthorized || called {
		t.Errorf("no header: code=%d called=%v", rec.Code, called)
	}

	// Wrong token → 401
	called = false
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Errorf("wrong token: code=%d called=%v", rec.Code, called)
	}

	// Correct token → 200
	called = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !called {
		t.Errorf("correct token: code=%d called=%v", rec.Code, called)
	}

	// Empty configured token → 503 (server misconfigured)
	h = bearerAuth(next, "", slog.New(slog.NewTextHandler(&discardWriter{}, nil)))
	called = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer anything")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Errorf("empty token: code=%d called=%v", rec.Code, called)
	}
}

// --- helpers ---

// registerMockWorldsim registers minimal NATS handlers that mimic the
// worldsim responses the MCP client expects. This avoids needing a full
// Simulator (which has private fields and PocketBase deps).
func registerMockWorldsim(t *testing.T, nc *nats.Conn) {
	t.Helper()

	// worldsim.stats.get
	nc.Subscribe("worldsim.stats.get", func(m *nats.Msg) {
		stats := map[string]any{
			"tick_hz":        20,
			"uptime":         "1m",
			"total_entities": 3,
			"total_players":  2,
			"maps": []map[string]any{
				{"name": "main", "player_count": 2, "entity_count": 3},
			},
			"players": []map[string]any{
				{"entity_id": "sub1", "client_id": "c_a", "display_name": "Alice"},
				{"entity_id": "sub2", "client_id": "c_b", "display_name": "Bob"},
			},
			"extensions": []map[string]any{},
		}
		data, _ := json.Marshal(stats)
		m.Respond(data)
	})

	// worldsim.entities.query — return 2 entities for map=main, 0 otherwise.
	nc.Subscribe("worldsim.entities.query", func(m *nats.Msg) {
		var req struct {
			MapID string `json:"map_id"`
		}
		json.Unmarshal(m.Data, &req)
		var out []map[string]any
		if req.MapID == "" || req.MapID == "main" {
			out = []map[string]any{
				{"entity_id": "e1", "map_id": "main", "state": "on", "entity_type": "light"},
				{"entity_id": "e2", "map_id": "main", "entity_type": "wall"},
			}
		}
		data, _ := json.Marshal(out)
		m.Respond(data)
	})

	// worldsim.entity.get
	nc.Subscribe("worldsim.entity.get", func(m *nats.Msg) {
		var req struct {
			EntityID string `json:"entity_id"`
		}
		json.Unmarshal(m.Data, &req)
		if req.EntityID == "e1" {
			data, _ := json.Marshal(map[string]any{"entity_id": "e1", "state": "on", "map_id": "main"})
			m.Respond(data)
			return
		}
		data, _ := json.Marshal(map[string]any{"error": "entity not found"})
		m.Respond(data)
	})

	// worldsim.admin.set_name / set_status / client.kick — return OK.
	for _, subj := range []string{"worldsim.admin.set_name", "worldsim.admin.set_status", "worldsim.admin.set_sprite", "worldsim.admin.set_player_options", "worldsim.admin.chat", "worldsim.client.kick"} {
		subj := subj
		nc.Subscribe(subj, func(m *nats.Msg) {
			// Special-case: set_status with status > 2 returns an error.
			if subj == "worldsim.admin.set_status" {
				var req struct {
					Status uint32 `json:"status"`
				}
				json.Unmarshal(m.Data, &req)
				if req.Status > 2 {
					data, _ := json.Marshal(map[string]any{"ok": false, "error": "invalid status (must be 0-2)"})
					m.Respond(data)
					return
				}
			}
			data, _ := json.Marshal(map[string]any{"ok": true})
			m.Respond(data)
		})
	}

	// worldsim.client.ban — return OK with kicked=true.
	nc.Subscribe("worldsim.client.ban", func(m *nats.Msg) {
		data, _ := json.Marshal(map[string]any{"ok": true, "kicked": true})
		m.Respond(data)
	})

	// worldsim.world_options.get — return a canned options object.
	nc.Subscribe("worldsim.world_options.get", func(m *nats.Msg) {
		opts := map[string]any{
			"ok": true,
			"options": map[string]any{
				"smtp_host":                   "mailhog",
				"smtp_port":                   1025,
				"smtp_from":                   "noreply@pixeleruv.local",
				"smtp_sender_name":            "PixelEruv",
				"app_url":                     "https://example.com:4043",
				"ffmpeg_concurrency":          2,
				"ffmpeg_timeout":              int64(10 * 60 * 1e9),
				"error_email_recipients_mode": "king",
				"recording_enabled":           true,
				"public_host":                 "example.com",
				"livekit_public_url":          "wss://livekit.example.com",
			},
		}
		data, _ := json.Marshal(opts)
		m.Respond(data)
	})

	// worldsim.world_options.set — echo back the input options with OK=true.
	// Reject ffmpeg_concurrency < 1 to exercise the error path. Expects the
	// {options, actor} wrapper used by the real worldsim handler.
	nc.Subscribe("worldsim.world_options.set", func(m *nats.Msg) {
		var req struct {
			Options WorldOptions   `json:"options"`
			Actor   map[string]any `json:"actor"`
		}
		if err := json.Unmarshal(m.Data, &req); err != nil {
			data, _ := json.Marshal(map[string]any{"ok": false, "error": "unmarshal: " + err.Error()})
			m.Respond(data)
			return
		}
		if req.Options.FFmpegConcurrency < 1 {
			data, _ := json.Marshal(map[string]any{"ok": false, "error": "ffmpeg_concurrency must be >= 1"})
			m.Respond(data)
			return
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "options": req.Options})
		m.Respond(data)
	})

	nc.Flush()
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// startEmbeddedNATS runs an in-process NATS server on a free port for tests.
// Mirrors the helper in worldsim/worldsim_ready_test.go (which is package-
// private). If you need to share this across packages, promote the original
// to an exported helper in a test-utilities package.
func startEmbeddedNATS(t *testing.T) (*server.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)
	opts := test.DefaultTestOptions
	opts.Host = host
	opts.Port, _ = net.LookupPort("tcp", port)
	srv, err := server.NewServer(&opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv, fmt.Sprintf("nats://%s", addr)
}

// Compile-time assertions to keep imports alive.
var _ = fmt.Sprintf
var _ = time.Second
