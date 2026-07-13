package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
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
		"bp": func() string { return basePath },
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
		"templates/player_timeline.html",
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
