package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

// Server is the audit service: NATS subscriber + HTTP UI.
type Server struct {
	nc             *nats.Conn
	store          EventStore
	logger         *slog.Logger
	healthzURL     string
	otelBaseURL    string
	dockerProxyURL string
	playerClient   *PlayerListClient
	basePath       string
	authUser       string
	authPass       string
	startTime      time.Time
	templates      map[string]*template.Template
	notifier       *notifier
}

func NewServer(nc *nats.Conn, store EventStore, logger *slog.Logger, healthzURL, otelBaseURL, dockerProxyURL, basePath, authUser, authPass string) (*Server, error) {
	tmpls, err := parseTemplates(basePath)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	s := &Server{
		nc:             nc,
		store:          store,
		logger:         logger,
		healthzURL:     healthzURL,
		otelBaseURL:    otelBaseURL,
		dockerProxyURL: dockerProxyURL,
		playerClient:   NewPlayerListClient(nc),
		basePath:       basePath,
		authUser:       authUser,
		authPass:       authPass,
		startTime:      time.Now(),
		templates:      tmpls,
		notifier:       newNotifier(nc, logger),
	}

	// Subscribe to audit events.
	if _, err := nc.Subscribe(audit.Subject, s.handleAuditEvent); err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", audit.Subject, err)
	}

	return s, nil
}

func (s *Server) Run(ctx context.Context, addr string) error {
	bp := s.basePath
	mux := http.NewServeMux()
	mux.HandleFunc(bp+"/", s.handleDashboard)
	mux.HandleFunc(bp+"/events", s.handleEvents)
	mux.HandleFunc(bp+"/events/", s.handleEventDetail)
	mux.HandleFunc(bp+"/players", s.handlePlayersList)
	mux.HandleFunc(bp+"/players/", s.handlePlayerDetail)
	mux.HandleFunc(bp+"/health", s.handleHealthPage)
	mux.HandleFunc(bp+"/docker/partial", s.handleDockerPartial)
	mux.HandleFunc(bp+"/world", s.handleWorld)
	mux.HandleFunc(bp+"/world/partial", s.handleWorldPartial)
	mux.HandleFunc(bp+"/healthz", s.handleHealthz)

	// JSON API endpoints (machine-readable mirrors of the HTML pages). Used
	// by the MCP server (backend/cmd/mcp) for historical audit queries; the
	// live event stream is delivered via the audit.event NATS subject.
	mux.HandleFunc(bp+"/api/events", s.handleAPIEvents)
	mux.HandleFunc(bp+"/api/events/", s.handleAPIEventDetail)
	mux.HandleFunc(bp+"/api/players", s.handleAPIPlayers)
	mux.HandleFunc(bp+"/api/players/", s.handleAPIPlayerDetail)
	mux.HandleFunc(bp+"/api/stats", s.handleAPIStats)

	// Static files (HTMX, CSS) — served from embedded filesystem.
	mux.Handle(bp+"/static/", http.StripPrefix(bp+"/static/", http.FileServer(http.FS(staticFilesystem()))))

	// Wrap with basic auth if credentials are configured.
	handler := http.Handler(mux)
	if s.authUser != "" && s.authPass != "" {
		handler = s.basicAuth(handler)
	}

	srv := &http.Server{Addr: addr, Handler: handler}

	go s.retentionLoop(ctx)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	s.logger.Info("audit service listening", "addr", addr, "version", version.Version)
	return srv.ListenAndServe()
}

// handleAuditEvent is the NATS callback that persists each event and, for
// SeverityError events, dispatches an error email via the notifier.
func (s *Server) handleAuditEvent(m *nats.Msg) {
	var ev audit.Event
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		s.logger.Warn("audit event unmarshal", "err", err)
		return
	}
	if err := s.store.Insert(ev); err != nil {
		s.logger.Warn("audit event insert", "err", err, "type", ev.EventType)
	}
	// Email on SeverityError only. Dispatch in a goroutine so persistence
	// (above) is never blocked on SMTP. The notifier is a no-op when mode
	// is "" or "none".
	if ev.Severity == audit.SeverityError && s.notifier != nil {
		go s.notifier.notify(ev)
	}
}

// retentionLoop deletes events older than the retention period. Default 30 days.
func (s *Server) retentionLoop(ctx context.Context) {
	retention := 30 * 24 * time.Hour
	if v := os.Getenv("AUDIT_RETENTION_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			retention = time.Duration(h) * time.Hour
		}
	}
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-retention)
			if n, err := s.store.DeleteOlderThan(cutoff); err != nil {
				s.logger.Warn("retention cleanup", "err", err)
			} else if n > 0 {
				s.logger.Info("retention cleanup", "deleted", n, "cutoff", cutoff.Format(time.RFC3339))
			}
		}
	}
}

// --- HTTP Handlers ---

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.basePath+"/" && r.URL.Path != s.basePath {
		http.NotFound(w, r)
		return
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	sevCounts, sevErr := s.store.CountBySeverity(since)
	if sevErr != nil {
		s.logger.Warn("dashboard CountBySeverity", "err", sevErr)
	}
	typeCounts, typeErr := s.store.CountByType(since)
	if typeErr != nil {
		s.logger.Warn("dashboard CountByType", "err", typeErr)
	}
	recent, qErr := s.store.Query(QueryFilter{Limit: 20})
	if qErr != nil {
		s.logger.Warn("dashboard Query", "err", qErr)
	}
	health := s.fetchHealthz()

	s.render(w, "dashboard.html", map[string]any{
		"BasePath":      s.basePath,
		"SeverityCounts": sevCounts,
		"TypeCounts":     typeCounts,
		"RecentEvents":   recent,
		"Health":         health,
		"OtelBaseURL":    s.otelBaseURL,
		"Uptime":         time.Since(s.startTime).Round(time.Second).String(),
		"Version":        version.Version,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	f := QueryFilter{
		EventType: r.URL.Query().Get("type"),
		Severity:  r.URL.Query().Get("severity"),
		ActorSub:  r.URL.Query().Get("actor"),
		EntityID:  r.URL.Query().Get("entity"),
		Limit:     50,
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			f.Limit = n
		}
	}
	events, err := s.store.Query(f)
	if err != nil {
		s.logger.Warn("query events", "err", err)
		http.Error(w, "query error", 500)
		return
	}

	// HTMX partial: return just the table fragment if HX-Request header is set.
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, "events_table.html", map[string]any{
			"Events":      events,
			"OtelBaseURL": s.otelBaseURL,
			"BasePath":    s.basePath,
		})
		return
	}

	s.render(w, "events.html", map[string]any{
		"Events":      events,
		"Filter":      f,
		"OtelBaseURL": s.otelBaseURL,
		"BasePath":    s.basePath,
	})
}

func (s *Server) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, s.basePath+"/events/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", 400)
		return
	}
	ev, err := s.store.GetByID(id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	s.render(w, "event_detail.html", map[string]any{
		"Event":       ev,
		"OtelBaseURL": s.otelBaseURL,
		"BasePath":    s.basePath,
	})
}

func (s *Server) handlePlayersList(w http.ResponseWriter, r *http.Request) {
	players := s.mergedPlayers(r.Context())
	s.render(w, "players.html", map[string]any{
		"Players":     players,
		"BasePath":    s.basePath,
		"OtelBaseURL": s.otelBaseURL,
	})
}

func (s *Server) handlePlayerDetail(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, s.basePath+"/players/")
	if sub == "" {
		http.Redirect(w, r, s.basePath+"/players", http.StatusFound)
		return
	}
	sessions, err := s.store.PlayerSessions(sub)
	if err != nil {
		s.logger.Warn("player sessions", "err", err, "sub", sub)
	}
	events, err := s.store.PlayerEvents(sub, 200)
	if err != nil {
		s.logger.Warn("player events", "err", err, "sub", sub)
	}
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	activityEvents, err := s.store.PlayerActivityEvents(sub, since)
	if err != nil {
		s.logger.Warn("player activity events", "err", err, "sub", sub)
	}
	now := time.Now().UTC()
	segments := buildActivitySegments(activityEvents, since, now)

	// Day boundaries for the SVG grid: each midnight between since and now.
	type dayMarker struct {
		Label string
		X     float64
	}
	var days []dayMarker
	dayStart := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, since.Location())
	for d := dayStart.Add(24 * time.Hour); d.Before(now); d = d.Add(24 * time.Hour) {
		days = append(days, dayMarker{
			Label: d.Format("Jan 02"),
			X:     float64(d.Sub(since)) / float64(now.Sub(since)) * 1000,
		})
	}

	// Summary stats.
	var totalSessionNs int64
	for _, sess := range sessions {
		totalSessionNs += int64(sess.Duration)
	}
	var displayName string
	for _, ev := range events {
		if ev.Actor.DisplayName != "" {
			displayName = ev.Actor.DisplayName
			break
		}
	}

	s.render(w, "player_detail.html", map[string]any{
		"PlayerSub":      sub,
		"DisplayName":    displayName,
		"Sessions":       sessions,
		"Events":         events,
		"ActivityEvents": activityEvents,
		"Segments":       segments,
		"DayMarkers":     days,
		"TimelineStart":  since,
		"TimelineEnd":    now,
		"TotalSessionNs": totalSessionNs,
		"SessionCount":   len(sessions),
		"EventCount":     len(events),
		"OtelBaseURL":    s.otelBaseURL,
		"BasePath":       s.basePath,
	})
}

// activitySegment is a colored bar in the 7-day SVG timeline. State is one
// of: "offline", "present", "busy", "dnd", "afk".
type activitySegment struct {
	Start time.Time
	End   time.Time
	State string
}

// buildActivitySegments walks the player's activity events chronologically and
// produces a sequence of colored segments covering [since, now]. The player
// starts offline; each connect/disconnect/status/afk event may transition the
// state. Segments are emitted on each state change.
func buildActivitySegments(events []StoredEvent, since, now time.Time) []activitySegment {
	type state struct {
		connected bool
		status    int // 0=present, 1=busy, 2=dnd
		afk       bool
	}
	cur := state{}
	curEnd := since
	stateName := func(st state) string {
		if !st.connected {
			return "offline"
		}
		if st.afk {
			return "afk"
		}
		switch st.status {
		case 1:
			return "busy"
		case 2:
			return "dnd"
		default:
			return "present"
		}
	}

	var segments []activitySegment
	emit := func(end time.Time) {
		if end.After(curEnd) {
			segments = append(segments, activitySegment{
				Start: curEnd,
				End:   end,
				State: stateName(cur),
			})
			curEnd = end
		}
	}

	for _, ev := range events {
		switch ev.EventType {
		case "client.connected":
			if !cur.connected {
				emit(ev.Timestamp)
				cur.connected = true
			}
		case "client.disconnected":
			if cur.connected {
				emit(ev.Timestamp)
				cur.connected = false
				cur.afk = false
			}
		case "player.set_status":
			var d struct{ Status int `json:"status"` }
			_ = json.Unmarshal(ev.Details, &d)
			if cur.connected && d.Status != cur.status {
				emit(ev.Timestamp)
				cur.status = d.Status
			}
		case "player.set_afk":
			var d struct{ Afk bool `json:"afk"` }
			_ = json.Unmarshal(ev.Details, &d)
			if cur.connected && d.Afk != cur.afk {
				emit(ev.Timestamp)
				cur.afk = d.Afk
			}
		}
	}
	emit(now)
	return segments
}

func (s *Server) handleHealthPage(w http.ResponseWriter, r *http.Request) {
	health := s.fetchHealthz()
	s.render(w, "health.html", map[string]any{
		"Health":   health,
		"Docker":   s.fetchDockerContainers(),
		"BasePath": s.basePath,
	})
}

// dockerContainerRow is one card in the docker section of /audit/health.
type dockerContainerRow struct {
	Name      string
	Image     string
	State     string
	Status    string
	Created   string
	StateKind string
}

// fetchDockerContainers queries the docker-readonly-proxy (which fronts the
// host docker socket with a strict GET /containers/json + GET /info
// allowlist) and returns one row per container in the pixeleruv compose
// project. Returns nil when DOCKER_PROXY_URL is not configured; returns an
// error row when the proxy is unreachable so the UI can surface it.
func (s *Server) fetchDockerContainers() []dockerContainerRow {
	if s.dockerProxyURL == "" {
		return nil
	}
	filters := `{"label":["com.docker.compose.project=pixeleruv"]}`
	u := s.dockerProxyURL + "/containers/json?all=true&filters=" + url.QueryEscape(filters)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		s.logger.Warn("docker proxy call", "err", err)
		return []dockerContainerRow{{Name: "docker-proxy unreachable", StateKind: "exited", Status: err.Error()}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		s.logger.Warn("docker proxy status", "status", resp.StatusCode)
		return []dockerContainerRow{{Name: "docker-proxy returned " + resp.Status, StateKind: "exited"}}
	}
	var containers []struct {
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
		Created int64    `json:"Created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		s.logger.Warn("docker containers decode", "err", err)
		return nil
	}
	rows := make([]dockerContainerRow, 0, len(containers))
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		rows = append(rows, dockerContainerRow{
			Name:      name,
			Image:     c.Image,
			State:     c.State,
			Status:    c.Status,
			Created:   dockerHumanCreated(c.Created),
			StateKind: dockerStateKind(c.State),
		})
	}
	return rows
}

// handleDockerPartial renders just the docker cards fragment for htmx
// polling on /audit/health. No-op (empty body) when DOCKER_PROXY_URL is
// not configured, so the section stays blank.
func (s *Server) handleDockerPartial(w http.ResponseWriter, r *http.Request) {
	rows := s.fetchDockerContainers()
	if rows == nil {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	data := map[string]any{"Docker": rows, "BasePath": s.basePath}
	t := s.templates["health.html"]
	if err := t.ExecuteTemplate(w, "docker_cards", data); err != nil {
		s.logger.Warn("render docker partial", "err", err)
	}
}

// dockerStateKind maps Docker's State field to a CSS color bucket.
func dockerStateKind(state string) string {
	switch state {
	case "running":
		return "running"
	case "exited", "dead":
		return "exited"
	case "created", "restarting", "paused", "removing":
		return "warn"
	default:
		return "other"
	}
}

// dockerHumanCreated formats a unix-seconds Created timestamp as
// "2006-01-02 15:04:05" in the local zone. Returns "" for 0.
func dockerHumanCreated(created int64) string {
	if created == 0 {
		return ""
	}
	return time.Unix(created, 0).Format("2006-01-02 15:04:05")
}

// fetchWorldStats requests world stats from worldsim via NATS request-reply.
// Returns the parsed stats map, or an error message string if the request failed.
func (s *Server) fetchWorldStats() (map[string]any, string) {
	reply, err := s.nc.Request("worldsim.stats.get", nil, 3*time.Second)
	if err != nil {
		s.logger.Warn("world stats request", "err", err)
		return nil, "worldsim unreachable: " + err.Error()
	}
	var stats map[string]any
	if err := json.Unmarshal(reply.Data, &stats); err != nil {
		s.logger.Warn("world stats parse", "err", err)
		return nil, "parse error: " + err.Error()
	}
	return stats, ""
}

func (s *Server) handleWorld(w http.ResponseWriter, r *http.Request) {
	stats, errMsg := s.fetchWorldStats()
	data := map[string]any{"BasePath": s.basePath}
	if errMsg != "" {
		data["Error"] = errMsg
	} else {
		data["Stats"] = stats
	}
	s.render(w, "world.html", data)
}

// handleWorldPartial renders just the world_content fragment for htmx polling.
func (s *Server) handleWorldPartial(w http.ResponseWriter, r *http.Request) {
	stats, errMsg := s.fetchWorldStats()
	data := map[string]any{"BasePath": s.basePath}
	if errMsg != "" {
		data["Error"] = errMsg
	} else {
		data["Stats"] = stats
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	t := s.templates["world.html"]
	if err := t.ExecuteTemplate(w, "world_content", data); err != nil {
		s.logger.Warn("render world partial", "err", err)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"service": "audit",
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(s.startTime).Round(time.Second).String(),
	})
}

// fetchHealthz fetches service health from the pusher's /healthz endpoint.
type healthEntry = audit.Details

func (s *Server) fetchHealthz() []map[string]any {
	if s.healthzURL == "" {
		return nil
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(s.healthzURL)
	if err != nil {
		return []map[string]any{{"service": "pusher", "status": "unreachable", "error": err.Error()}}
	}
	defer resp.Body.Close()
	var result struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	return result.Services
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	t, ok := s.templates[name]
	if !ok {
		s.logger.Warn("template not found", "name", name)
		http.Error(w, "template not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		s.logger.Warn("render template", "name", name, "err", err)
	}
}

// basicAuth wraps an http.Handler with HTTP Basic Auth. /healthz and /static/
// are exempt so health checks and CSS/JS load without credentials.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exempt healthz and static files from auth.
		if r.URL.Path == s.basePath+"/healthz" ||
			strings.HasPrefix(r.URL.Path, s.basePath+"/static/") {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != s.authUser || pass != s.authPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="PixelEruv Audit"`)
			http.Error(w, "Unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- JSON API handlers (machine-readable mirrors of the HTML pages) ---
//
// These endpoints exist so the MCP server (backend/cmd/mcp) can query audit
// history without coupling to the SQLite storage backend. They mirror the
// HTML handlers but return application/json. Live events are delivered via
// the audit.event NATS subject, not these endpoints.

// apiEvent is the JSON shape returned by the audit API. It is the same as
// StoredEvent but with a stable, exported timestamp string for clients that
// don't parse time.Time.
type apiEvent struct {
	ID        int64           `json:"id"`
	EventType string          `json:"event_type"`
	Severity  string          `json:"severity"`
	Timestamp string          `json:"timestamp"`
	Actor     audit.Actor     `json:"actor"`
	Details   json.RawMessage `json:"details"`
	TraceID   string          `json:"trace_id,omitempty"`
}

func toAPIEvent(se StoredEvent) apiEvent {
	return apiEvent{
		ID:        se.ID,
		EventType: se.EventType,
		Severity:  se.Severity,
		Timestamp: se.Timestamp.UTC().Format(time.RFC3339),
		Actor:     se.Actor,
		Details:   se.Details,
		TraceID:   se.TraceID,
	}
}

func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	f := QueryFilter{
		EventType: r.URL.Query().Get("type"),
		Severity:  r.URL.Query().Get("severity"),
		ActorSub:  r.URL.Query().Get("actor"),
		EntityID:  r.URL.Query().Get("entity"),
		Limit:     50,
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			f.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			f.Offset = n
		}
	}
	events, err := s.store.Query(f)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	out := make([]apiEvent, 0, len(events))
	for _, se := range events {
		out = append(out, toAPIEvent(se))
	}
	writeAPIJSON(w, out)
}

func (s *Server) handleAPIEventDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, s.basePath+"/api/events/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad id")
		return
	}
	se, err := s.store.GetByID(id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	writeAPIJSON(w, toAPIEvent(se))
}

// apiPlayerSummary is the JSON shape for one row in the /api/players list.
type apiPlayerSummary struct {
	Sub             string `json:"sub"`
	DisplayName     string `json:"display_name"`
	FirstSeen       string `json:"first_seen"`
	LastSeen        string `json:"last_seen"`
	EventCount      int    `json:"event_count"`
	ConnectCount    int    `json:"connect_count"`
	TotalSessionSec int64  `json:"total_session_sec"`
	Created         string `json:"created,omitempty"`
	IsAdmin         bool   `json:"is_admin"`
}

// mergedPlayers fetches audit stats and PocketBase records, merges them, and
// returns a sorted leaderboard. Players with no audit events still appear
// (from PB) with zero stats. Falls back to audit-only data if PB is not
// configured.
func (s *Server) mergedPlayers(ctx context.Context) []PlayerSummary {
	auditPlayers, err := s.store.ListPlayers()
	if err != nil {
		s.logger.Warn("list players", "err", err)
		return nil
	}
	auditBySub := make(map[string]*PlayerSummary, len(auditPlayers))
	for i := range auditPlayers {
		auditBySub[auditPlayers[i].Sub] = &auditPlayers[i]
	}

	pbPlayers, pbErr := s.playerClient.ListPlayers(ctx)
	if pbErr != nil {
		s.logger.Warn("list players from worldsim", "err", pbErr)
	}

	if len(pbPlayers) == 0 {
		return auditPlayers
	}

	merged := make([]PlayerSummary, 0, len(pbPlayers))
	for _, pb := range pbPlayers {
		if pb.UserID == "" || pb.UserID == "dev" {
			continue
		}
		ps := PlayerSummary{
			Sub:         pb.UserID,
			DisplayName: pb.DisplayName,
			IsAdmin:     pb.IsAdmin,
		}
		if pb.Created != "" {
			ps.Created, _ = time.Parse(time.RFC3339, pb.Created)
		}
		if stats, ok := auditBySub[pb.UserID]; ok {
			ps.FirstSeen = stats.FirstSeen
			ps.LastSeen = stats.LastSeen
			ps.EventCount = stats.EventCount
			ps.ConnectCount = stats.ConnectCount
			ps.TotalSessionNs = stats.TotalSessionNs
			if ps.DisplayName == "" {
				ps.DisplayName = stats.DisplayName
			}
		}
		merged = append(merged, ps)
	}
	sort.Slice(merged, func(i, j int) bool {
		if (merged[i].TotalSessionNs > 0) != (merged[j].TotalSessionNs > 0) {
			return merged[i].TotalSessionNs > 0
		}
		if merged[i].TotalSessionNs != merged[j].TotalSessionNs {
			return merged[i].TotalSessionNs > merged[j].TotalSessionNs
		}
		return merged[i].Created.After(merged[j].Created)
	})
	return merged
}

func (s *Server) handleAPIPlayers(w http.ResponseWriter, r *http.Request) {
	players := s.mergedPlayers(r.Context())
	out := make([]apiPlayerSummary, 0, len(players))
	for _, p := range players {
		row := apiPlayerSummary{
			Sub:             p.Sub,
			DisplayName:     p.DisplayName,
			FirstSeen:       p.FirstSeen.UTC().Format(time.RFC3339),
			LastSeen:        p.LastSeen.UTC().Format(time.RFC3339),
			EventCount:      p.EventCount,
			ConnectCount:    p.ConnectCount,
			TotalSessionSec: p.TotalSessionNs / int64(time.Second),
			IsAdmin:         p.IsAdmin,
		}
		if !p.Created.IsZero() {
			row.Created = p.Created.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeAPIJSON(w, out)
}

// apiSession is the JSON shape for one session in the player detail response.
type apiSession struct {
	ClientID       string `json:"client_id"`
	ConnectedAt    string `json:"connected_at"`
	DisconnectedAt string `json:"disconnected_at,omitempty"`
	DurationSec    int64  `json:"duration_sec"`
	Open           bool   `json:"open"`
}

// apiPlayerDetail is the JSON shape returned by /api/players/{sub}.
type apiPlayerDetail struct {
	Sub          string     `json:"sub"`
	DisplayName  string     `json:"display_name"`
	Sessions     []apiSession `json:"sessions"`
	Events       []apiEvent   `json:"events"`
	ActivityEvents []apiEvent `json:"activity_events"`
}

func (s *Server) handleAPIPlayerDetail(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, s.basePath+"/api/players/")
	if sub == "" {
		writeAPIError(w, http.StatusBadRequest, "missing sub")
		return
	}
	sessions, err := s.store.PlayerSessions(sub)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "sessions failed: "+err.Error())
		return
	}
	events, err := s.store.PlayerEvents(sub, 200)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "events failed: "+err.Error())
		return
	}
	sinceHours := 7 * 24
	if v := r.URL.Query().Get("since_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 24*365 {
			sinceHours = n
		}
	}
	since := time.Now().UTC().Add(-time.Duration(sinceHours) * time.Hour)
	activityEvents, err := s.store.PlayerActivityEvents(sub, since)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "activity failed: "+err.Error())
		return
	}

	var displayName string
	for _, ev := range events {
		if ev.Actor.DisplayName != "" {
			displayName = ev.Actor.DisplayName
			break
		}
	}

	sessionsOut := make([]apiSession, 0, len(sessions))
	for _, sess := range sessions {
		row := apiSession{
			ClientID:    sess.ClientID,
			ConnectedAt: sess.ConnectedAt.UTC().Format(time.RFC3339),
			DurationSec: int64(sess.Duration / time.Second),
			Open:        sess.Open,
		}
		if !sess.Open {
			row.DisconnectedAt = sess.DisconnectedAt.UTC().Format(time.RFC3339)
		}
		sessionsOut = append(sessionsOut, row)
	}
	eventsOut := make([]apiEvent, 0, len(events))
	for _, se := range events {
		eventsOut = append(eventsOut, toAPIEvent(se))
	}
	activityOut := make([]apiEvent, 0, len(activityEvents))
	for _, se := range activityEvents {
		activityOut = append(activityOut, toAPIEvent(se))
	}

	writeAPIJSON(w, apiPlayerDetail{
		Sub:            sub,
		DisplayName:    displayName,
		Sessions:       sessionsOut,
		Events:         eventsOut,
		ActivityEvents: activityOut,
	})
}

// apiStats is the JSON shape returned by /api/stats: severity + type counts
// for the last 24h, plus service uptime and version.
type apiStats struct {
	Uptime         string         `json:"uptime"`
	Version        string         `json:"version"`
	SeverityCounts map[string]int `json:"severity_counts_24h"`
	TypeCounts     map[string]int `json:"type_counts_24h"`
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	sevCounts, sevErr := s.store.CountBySeverity(since)
	if sevErr != nil {
		s.logger.Warn("api stats CountBySeverity", "err", sevErr)
		sevCounts = map[string]int{}
	}
	typeCounts, typeErr := s.store.CountByType(since)
	if typeErr != nil {
		s.logger.Warn("api stats CountByType", "err", typeErr)
		typeCounts = map[string]int{}
	}
	writeAPIJSON(w, apiStats{
		Uptime:         time.Since(s.startTime).Round(time.Second).String(),
		Version:        version.Version,
		SeverityCounts: sevCounts,
		TypeCounts:     typeCounts,
	})
}

func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

func writeAPIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}
