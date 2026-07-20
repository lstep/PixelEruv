package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerResources wires MCP resources (URI-addressable, read-only) onto the
// server. Resources are the read counterpart to tools: an MCP client can list
// them and read them by URI without passing arguments.
//
// URIs use the pixeleruv:// scheme. Static resources are registered with
// AddResource; parameterized ones use AddResourceTemplate.
func registerResources(s *mcp.Server, w *WorldsimClient, a *AuditClient, pb *PocketBaseClient, d *DockerClient) {
	// Static resources (no path-template variables).
	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://world/stats",
		Name:        "World Stats",
		Description: "Live worldsim snapshot (tick rate, players, entities, extensions, per-map counts).",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) { return w.GetStats(ctx) }))

	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://world/zones",
		Name:        "World Zones",
		Description: "Zone metadata for all maps (id, type, shape, AV flags, portal targets).",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) { return w.GetZones(ctx) }))

	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://world/players",
		Name:        "Online Players",
		Description: "Currently-connected players (entity_id, client_id, display_name, map, x/y, is_admin, is_guest, IP).",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) {
		stats, err := w.GetStats(ctx)
		if err != nil {
			return nil, err
		}
		return stats.Players, nil
	}))

	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://world/extensions",
		Name:        "Extensions",
		Description: "Registered extensions and their alive/heartbeat state.",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) {
		stats, err := w.GetStats(ctx)
		if err != nil {
			return nil, err
		}
		return stats.Extensions, nil
	}))

	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://audit/stats",
		Name:        "Audit Stats (24h)",
		Description: "Severity and type counts for the last 24h of audit events, plus audit service uptime and version.",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) { return a.Stats(ctx) }))

	// Docker resources (via docker-readonly-proxy). Default to the pixeleruv
	// compose project; the resource has no args so the all_projects filter is
	// not exposed here — use the list_docker_containers tool with
	// all_projects=true for that.
	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://docker/containers",
		Name:        "Docker Containers",
		Description: "Running + stopped containers in the pixeleruv compose project (name, image, state, status, created, labels).",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) { return d.ListContainers(ctx, false) }))

	s.AddResource(&mcp.Resource{
		URI:         "pixeleruv://docker/info",
		Name:        "Docker Engine Info",
		Description: "Raw Docker engine info (container/image counts, OS, kernel, docker root dir).",
		MIMEType:    "application/json",
	}, makeStaticResourceHandler(func(ctx context.Context) (any, error) { return d.Info(ctx) }))

	// Parameterized resources (templates).
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://world/maps/{name}",
		Description: "A single map's stats (dimensions, player/entity/zone counts, zones).",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, name string) (any, error) {
		stats, err := w.GetStats(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range stats.Maps {
			if m.Name == name {
				return m, nil
			}
		}
		return nil, mcp.ResourceNotFoundError(fmt.Sprintf("pixeleruv://world/maps/%s", name))
	}))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://world/entities/{id}",
		Description: "A single entity snapshot by ID.",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, id string) (any, error) {
		return w.GetEntity(ctx, id)
	}))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://audit/events/{id}",
		Description: "A single audit event by ID.",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, idStr string) (any, error) {
		var id int64
		if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
			return nil, fmt.Errorf("event id must be an integer, got %q", idStr)
		}
		return a.GetEvent(ctx, id)
	}))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://audit/players/{sub}",
		Description: "Audit event timeline for a player (by OIDC subject).",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, sub string) (any, error) {
		return a.PlayerTimeline(ctx, sub)
	}))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://pb/{collection}",
		Description: "List records in a PocketBase collection (first page, 30 items).",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, collection string) (any, error) {
		return pb.ListRecords(ctx, collection, ListParams{PerPage: 30, Page: 1})
	}))

	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "pixeleruv://pb/{collection}/{id}",
		Description: "A single PocketBase record by collection + ID.",
		MIMEType:    "application/json",
	}, makeTemplatedResourceHandler(func(ctx context.Context, path string) (any, error) {
		// path is "collection/id"
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("expected collection/id, got %q", path)
		}
		return pb.GetRecord(ctx, parts[0], parts[1])
	}))
}

// makeStaticResourceHandler adapts a no-arg getter into an mcp.ResourceHandler
// for static resources (no path-template variables).
func makeStaticResourceHandler(get func(ctx context.Context) (any, error)) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		v, err := get(ctx)
		if err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal resource: %w", err)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: string(data)},
			},
		}, nil
	}
}

// makeTemplatedResourceHandler adapts a single-string-arg getter into an
// mcp.ResourceHandler for parameterized resources. The string argument is the
// last path segment of the URI (or "collection/id" for the PB two-segment
// template, handled by the handler itself).
func makeTemplatedResourceHandler(get func(ctx context.Context, arg string) (any, error)) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		arg, err := extractLastSegment(req.Params.URI)
		if err != nil {
			return nil, err
		}
		v, err := get(ctx, arg)
		if err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal resource: %w", err)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: string(data)},
			},
		}, nil
	}
}

// extractLastSegment returns the variable portion of a templated URI. For
// single-variable templates (pixeleruv://world/maps/{name}) it returns the
// last path segment. For the PB two-segment template
// (pixeleruv://pb/{collection}/{id}) it returns "collection/id".
func extractLastSegment(uri string) (string, error) {
	rest := strings.TrimPrefix(uri, "pixeleruv://")
	segments := strings.Split(rest, "/")
	if len(segments) < 2 {
		return "", fmt.Errorf("missing path segment in %s", uri)
	}
	// pb/{collection}/{id} → return "collection/id"
	if segments[0] == "pb" && len(segments) >= 3 {
		return strings.Join(segments[1:], "/"), nil
	}
	// Single-variable template → last segment
	return segments[len(segments)-1], nil
}
