package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/version"
	"github.com/nats-io/nats.go"
)

// Server is the audit service: NATS subscriber + HTTP UI.
type Server struct {
	nc          *nats.Conn
	store       EventStore
	logger      *slog.Logger
	healthzURL  string
	otelBaseURL string
	basePath    string
	authUser    string
	authPass    string
	startTime   time.Time
	templates   map[string]*template.Template
}

func NewServer(nc *nats.Conn, store EventStore, logger *slog.Logger, healthzURL, otelBaseURL, basePath, authUser, authPass string) (*Server, error) {
	tmpls, err := parseTemplates(basePath)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	s := &Server{
		nc:          nc,
		store:       store,
		logger:      logger,
		healthzURL:  healthzURL,
		otelBaseURL: otelBaseURL,
		basePath:    basePath,
		authUser:    authUser,
		authPass:    authPass,
		startTime:   time.Now(),
		templates:   tmpls,
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
	mux.HandleFunc(bp+"/players/", s.handlePlayerTimeline)
	mux.HandleFunc(bp+"/health", s.handleHealthPage)
	mux.HandleFunc(bp+"/world", s.handleWorld)
	mux.HandleFunc(bp+"/world/partial", s.handleWorldPartial)
	mux.HandleFunc(bp+"/healthz", s.handleHealthz)

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

// handleAuditEvent is the NATS callback that persists each event.
func (s *Server) handleAuditEvent(m *nats.Msg) {
	var ev audit.Event
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		s.logger.Warn("audit event unmarshal", "err", err)
		return
	}
	if err := s.store.Insert(ev); err != nil {
		s.logger.Warn("audit event insert", "err", err, "type", ev.EventType)
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

func (s *Server) handlePlayerTimeline(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, s.basePath+"/players/")
	if sub == "" {
		http.Error(w, "missing sub", 400)
		return
	}
	events, err := s.store.Query(QueryFilter{ActorSub: sub, Limit: 200})
	if err != nil {
		s.logger.Warn("query player timeline", "err", err)
		http.Error(w, "query error", 500)
		return
	}
	s.render(w, "player_timeline.html", map[string]any{
		"PlayerSub":   sub,
		"Events":      events,
		"OtelBaseURL": s.otelBaseURL,
		"BasePath":    s.basePath,
	})
}

func (s *Server) handleHealthPage(w http.ResponseWriter, r *http.Request) {
	health := s.fetchHealthz()
	s.render(w, "health.html", map[string]any{
		"Health":   health,
		"BasePath": s.basePath,
	})
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
