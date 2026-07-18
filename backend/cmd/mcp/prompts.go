package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerPrompts wires MCP prompts — pre-baked prompt templates the MCP
// client can render and send to an LLM. Each prompt fetches live data and
// returns it as a user-role message so the LLM has fresh context.
func registerPrompts(s *mcp.Server, w *WorldsimClient, a *AuditClient) {
	// summarize_recent_audit: last N events grouped by severity/type.
	s.AddPrompt(&mcp.Prompt{
		Name:        "summarize_recent_audit",
		Description: "Summarize recent audit activity: fetch the last N events, group by severity and event type, and present the summary to the LLM.",
		Arguments: []*mcp.PromptArgument{
			{Name: "limit", Description: "Number of recent events to fetch (default 50, max 500)", Required: false},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		limit := 50
		if v, ok := req.Params.Arguments["limit"]; ok && v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		events, err := a.QueryEvents(ctx, AuditQuery{Limit: limit})
		if err != nil {
			return nil, err
		}
		bySeverity := map[string]int{"info": 0, "warn": 0, "error": 0}
		byType := map[string]int{}
		var lines []string
		for _, ev := range events {
			bySeverity[ev.Severity]++
			byType[ev.EventType]++
			if len(lines) < 20 {
				lines = append(lines, fmt.Sprintf("- %s %s [%s] actor=%s details=%s",
					ev.Timestamp, ev.EventType, ev.Severity, actorString(ev.Actor), string(ev.Details)))
			}
		}
		summary := fmt.Sprintf(
			"Summarized %d recent audit events.\n\nBy severity: info=%d warn=%d error=%d\n\nBy type (top): %s\n\nFirst 20 events:\n%s",
			len(events), bySeverity["info"], bySeverity["warn"], bySeverity["error"],
			topTypes(byType), strings.Join(lines, "\n"))
		return &mcp.GetPromptResult{
			Description: "Recent audit activity summary",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: summary}},
			},
		}, nil
	})

	// investigate_player: timeline + current world state + PB record.
	s.AddPrompt(&mcp.Prompt{
		Name:        "investigate_player",
		Description: "Investigate a player by OIDC subject: pull their audit timeline, current world state (if online), and PocketBase record. Present all three to the LLM for triage.",
		Arguments: []*mcp.PromptArgument{
			{Name: "sub", Description: "Player OIDC subject", Required: true},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		sub := req.Params.Arguments["sub"]
		if sub == "" {
			return nil, fmt.Errorf("sub argument is required")
		}
		timeline, err := a.PlayerTimeline(ctx, sub)
		if err != nil {
			return nil, fmt.Errorf("player timeline: %w", err)
		}
		stats, statsErr := w.GetStats(ctx)
		var online *PlayerStats
		if statsErr == nil {
			for _, p := range stats.Players {
				if p.EntityID == sub {
					online = &p
					break
				}
			}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Player investigation for sub=%s\n\n", sub)
		fmt.Fprintf(&sb, "Audit timeline (%d events):\n", len(timeline))
		for _, ev := range timeline {
			fmt.Fprintf(&sb, "- %s %s [%s] details=%s\n", ev.Timestamp, ev.EventType, ev.Severity, string(ev.Details))
		}
		if online != nil {
			fmt.Fprintf(&sb, "\nCurrently online: entity_id=%s client_id=%s map=%s x=%.0f y=%.0f is_admin=%v is_guest=%v ip=%s\n",
				online.EntityID, online.ClientID, online.MapID, online.X, online.Y, online.IsAdmin, online.IsGuest, online.IP)
		} else if statsErr != nil {
			fmt.Fprintf(&sb, "\nWorldsim unreachable, cannot check online status: %v\n", statsErr)
		} else {
			fmt.Fprintf(&sb, "\nNot currently online.\n")
		}
		return &mcp.GetPromptResult{
			Description: "Player investigation: timeline + online state",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: sb.String()}},
			},
		}, nil
	})

	// world_health_report: stats + extension status + recent warn/error events.
	s.AddPrompt(&mcp.Prompt{
		Name:        "world_health_report",
		Description: "Generate a world health report: worldsim stats (tick rate, uptime, player/entity counts), extension alive status, and recent warn/error audit events. Present to the LLM for assessment.",
		Arguments:   []*mcp.PromptArgument{},
	}, func(ctx context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		stats, err := w.GetStats(ctx)
		if err != nil {
			return nil, fmt.Errorf("world stats: %w", err)
		}
		warns, _ := a.QueryEvents(ctx, AuditQuery{Severity: "warn", Limit: 20})
		errors, _ := a.QueryEvents(ctx, AuditQuery{Severity: "error", Limit: 20})

		var sb strings.Builder
		fmt.Fprintf(&sb, "World Health Report\n\n")
		fmt.Fprintf(&sb, "worldsim: tick_hz=%d uptime=%s tick_count=%d total_players=%d total_entities=%d\n",
			stats.TickHz, stats.Uptime, stats.TickCount, stats.TotalPlayers, stats.TotalEntities)
		fmt.Fprintf(&sb, "\nExtensions (%d):\n", len(stats.Extensions))
		for _, e := range stats.Extensions {
			status := "alive"
			if !e.Alive {
				status = "STALE"
			}
			fmt.Fprintf(&sb, "- %s: %s (heartbeat_age=%s, input_triggers=%d, gate_triggers=%d)\n",
				e.ID, status, e.HeartbeatAge, e.InputTriggers, e.GateTriggers)
		}
		fmt.Fprintf(&sb, "\nMaps (%d):\n", len(stats.Maps))
		for _, m := range stats.Maps {
			fmt.Fprintf(&sb, "- %s: %dx%d players=%d entities=%d zones=%d\n",
				m.Name, m.Width, m.Height, m.PlayerCount, m.EntityCount, m.ZoneCount)
		}
		fmt.Fprintf(&sb, "\nRecent warnings (%d):\n", len(warns))
		for _, ev := range warns {
			fmt.Fprintf(&sb, "- %s %s details=%s\n", ev.Timestamp, ev.EventType, string(ev.Details))
		}
		fmt.Fprintf(&sb, "\nRecent errors (%d):\n", len(errors))
		for _, ev := range errors {
			fmt.Fprintf(&sb, "- %s %s details=%s\n", ev.Timestamp, ev.EventType, string(ev.Details))
		}
		return &mcp.GetPromptResult{
			Description: "World health report",
			Messages: []*mcp.PromptMessage{
				{Role: "user", Content: &mcp.TextContent{Text: sb.String()}},
			},
		}, nil
	})
}

// actorString renders an audit.Actor as a short string for prompt summaries.
func actorString(a audit.Actor) string {
	parts := []string{}
	if a.Sub != "" {
		parts = append(parts, "sub="+a.Sub)
	}
	if a.EntityID != "" {
		parts = append(parts, "entity="+a.EntityID)
	}
	if a.ClientID != "" {
		parts = append(parts, "client="+a.ClientID)
	}
	if a.Extension != "" {
		parts = append(parts, "ext="+a.Extension)
	}
	if a.IP != "" {
		parts = append(parts, "ip="+a.IP)
	}
	if len(parts) == 0 {
		return "(unknown)"
	}
	return strings.Join(parts, " ")
}

// topTypes returns a string listing the most common event types.
func topTypes(counts map[string]int) string {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	// Simple insertion sort by count desc; counts is small.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].v > all[j-1].v; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	var parts []string
	for i, kv := range all {
		if i >= 10 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", kv.k, kv.v))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " ")
}
