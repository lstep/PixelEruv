package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// parseTemplates loads the base template, then parses each page template in
// its own template set (cloned from the base) so that {{define "title"}} and
// {{define "content"}} blocks from different pages don't collide.
//
// A "bp" template function is registered so templates can emit the base path
// without relying on $.BasePath (which breaks when a sub-template is called
// with a slice or non-struct data).
func parseTemplates(basePath string) (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"bp":           func() string { return basePath },
		"flagClass":    flagClassFor,
		"add":          func(a, b int) int { return a + b },
		"durationStr":  durationStr,
		"segmentClass": segmentClassFor,
		"segmentX":     segmentX,
		"segmentW":     segmentW,
	}

	// Parse the base template (defines "base", "events_table_inner", "world_content").
	baseTmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS,
		"templates/base.html",
		"templates/events_table.html",
		"templates/world_content.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse base: %w", err)
	}

	// Page templates that define "title" and "content" blocks.
	pages := []string{
		"templates/dashboard.html",
		"templates/events.html",
		"templates/event_detail.html",
		"templates/players.html",
		"templates/player_detail.html",
		"templates/world.html",
		"templates/health.html",
	}

	templates := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		clone, err := baseTmpl.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone for %s: %w", page, err)
		}
		t, err := clone.ParseFS(templatesFS, page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		// Use the full filename (e.g. "dashboard.html") as the key.
		name := page[len("templates/"):]
		templates[name] = t
	}
	return templates, nil
}

// staticFilesystem returns the embedded static files as an http.FileSystem.
func staticFilesystem() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return sub
}

// durationStr formats a duration as a human-readable string like "2h 15m" or
// "3d 4h". Accepts time.Duration (from Session.Duration) or int64 nanoseconds
// (from PlayerSummary.TotalSessionNs).
func durationStr(v any) string {
	var d time.Duration
	switch val := v.(type) {
	case time.Duration:
		d = val
	case int64:
		d = time.Duration(val)
	default:
		return "0m"
	}
	if d <= 0 {
		return "0m"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// segmentClassFor returns the CSS class for an activity segment state.
func segmentClassFor(state string) string {
	return "act-" + state
}

// segmentX computes the SVG x coordinate (0-1000) for a segment start within
// the [timelineStart, timelineEnd] range.
func segmentX(start, timelineStart, timelineEnd time.Time) float64 {
	total := timelineEnd.Sub(timelineStart)
	if total <= 0 {
		return 0
	}
	offset := start.Sub(timelineStart)
	return float64(offset) / float64(total) * 1000
}

// segmentW computes the SVG width (0-1000) for a segment within the
// [timelineStart, timelineEnd] range.
func segmentW(start, end, timelineStart, timelineEnd time.Time) float64 {
	total := timelineEnd.Sub(timelineStart)
	if total <= 0 {
		return 0
	}
	w := float64(end.Sub(start)) / float64(total) * 1000
	if w < 1 {
		w = 1 // ensure visible even for very short segments
	}
	return w
}
