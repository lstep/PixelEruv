package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats.go"
)

// AuditClient combines two access paths to audit data:
//   - HTTP GET to the audit service's JSON API (/audit/api/events,
//     /audit/api/events/{id}, /audit/api/players/{sub}, /audit/api/stats) for
//     historical queries.
//   - NATS subscription to "audit.event" for live event notifications
//     forwarded to MCP clients.
//
// Both paths are optional: if AuditBaseURL is empty, HTTP methods return an
// error; if nc is nil, the live subscription is skipped.
type AuditClient struct {
	baseURL    string // e.g. "http://audit:8082/audit" (no trailing slash)
	httpClient *http.Client
	basicUser  string // optional basic auth
	basicPass  string
	nc         *nats.Conn
}

func NewAuditClient(baseURL, basicUser, basicPass string, nc *nats.Conn) *AuditClient {
	return &AuditClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		basicUser:  basicUser,
		basicPass:  basicPass,
		nc:         nc,
	}
}

// APIEvent mirrors the audit service's apiEvent JSON shape.
type APIEvent struct {
	ID        int64           `json:"id"`
	EventType string          `json:"event_type"`
	Severity  string          `json:"severity"`
	Timestamp string          `json:"timestamp"`
	Actor     audit.Actor     `json:"actor"`
	Details   json.RawMessage `json:"details"`
	TraceID   string          `json:"trace_id,omitempty"`
}

type AuditQuery struct {
	EventType string
	Severity  string
	ActorSub  string
	EntityID  string
	Limit     int
	Offset    int
}

func (q *AuditQuery) toURLValues() url.Values {
	v := url.Values{}
	if q.EventType != "" {
		v.Set("type", q.EventType)
	}
	if q.Severity != "" {
		v.Set("severity", q.Severity)
	}
	if q.ActorSub != "" {
		v.Set("actor", q.ActorSub)
	}
	if q.EntityID != "" {
		v.Set("entity", q.EntityID)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	return v
}

func (a *AuditClient) getJSON(ctx context.Context, path string) ([]byte, error) {
	if a.baseURL == "" {
		return nil, fmt.Errorf("audit base URL not configured (AUDIT_BASE_URL)")
	}
	full := a.baseURL + path
	req, err := http.NewRequestWithContext(ctx, "GET", full, nil)
	if err != nil {
		return nil, err
	}
	if a.basicUser != "" || a.basicPass != "" {
		req.SetBasicAuth(a.basicUser, a.basicPass)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("audit GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("audit read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audit %s: HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

func (a *AuditClient) QueryEvents(ctx context.Context, q AuditQuery) ([]APIEvent, error) {
	path := "/api/events?" + q.toURLValues().Encode()
	data, err := a.getJSON(ctx, path)
	if err != nil {
		return nil, err
	}
	var out []APIEvent
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal audit events: %w", err)
	}
	return out, nil
}

func (a *AuditClient) GetEvent(ctx context.Context, id int64) (*APIEvent, error) {
	data, err := a.getJSON(ctx, fmt.Sprintf("/api/events/%d", id))
	if err != nil {
		return nil, err
	}
	var ev APIEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("unmarshal audit event: %w", err)
	}
	return &ev, nil
}

func (a *AuditClient) PlayerTimeline(ctx context.Context, sub string) ([]APIEvent, error) {
	data, err := a.getJSON(ctx, "/api/players/"+url.PathEscape(sub))
	if err != nil {
		return nil, err
	}
	var out []APIEvent
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal player timeline: %w", err)
	}
	return out, nil
}

// AuditStats mirrors the audit service's apiStats JSON shape.
type AuditStats struct {
	Uptime         string         `json:"uptime"`
	Version        string         `json:"version"`
	SeverityCounts map[string]int `json:"severity_counts_24h"`
	TypeCounts     map[string]int `json:"type_counts_24h"`
}

func (a *AuditClient) Stats(ctx context.Context) (*AuditStats, error) {
	data, err := a.getJSON(ctx, "/api/stats")
	if err != nil {
		return nil, err
	}
	var s AuditStats
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal audit stats: %w", err)
	}
	return &s, nil
}

// SubscribeLive subscribes to the audit.event NATS subject and invokes cb for
// each event. Returns an error if NATS is not configured. The subscription
// runs until ctx is canceled.
func (a *AuditClient) SubscribeLive(ctx context.Context, cb func(event audit.Event)) error {
	if a.nc == nil {
		return fmt.Errorf("NATS connection not configured for live audit events")
	}
	sub, err := a.nc.Subscribe("audit.event", func(m *nats.Msg) {
		var ev audit.Event
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		cb(ev)
	})
	if err != nil {
		return fmt.Errorf("subscribe audit.event: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	return nil
}
