package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DockerClient is a minimal HTTP client for the docker-readonly-proxy. The
// proxy fronts the host Docker engine socket with a strict GET /containers/json
// + GET /info allowlist, so this client only exposes those two calls. Used by
// the MCP server to expose running-container status to MCP clients.
//
// If baseURL is empty (DOCKER_PROXY_URL unset), methods return an error — the
// rest of the MCP server still works, mirroring the optional Audit/PocketBase
// client pattern.
type DockerClient struct {
	baseURL    string
	httpClient *http.Client
}

func NewDockerClient(baseURL string) *DockerClient {
	return &DockerClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ContainerRow is a cleaned-up view of one Docker container, suitable for
// returning from MCP tools. Mirrors the fields the admin /admin/docker page
// uses (backend/cmd/admin/server.go dockerContainerRow) plus the compose
// project + service labels for filtering/grouping.
type ContainerRow struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`        // first name without leading "/"
	Image     string            `json:"image"`
	ImageID   string            `json:"image_id,omitempty"`
	State     string            `json:"state"`      // running|created|exited|paused|restarting|...
	Status    string            `json:"status"`     // Docker's human string: "Up 5 minutes", "Exited (0) ..."
	Created   int64             `json:"created"`    // unix seconds
	Labels    map[string]string `json:"labels,omitempty"`
}

// ListContainers calls GET /containers/json on the docker-readonly-proxy. By
// default it filters to the pixeleruv compose project
// (com.docker.compose.project=pixeleruv); pass allProjects=true to return
// every container on the host engine. all=true is always set so stopped
// containers are included.
func (d *DockerClient) ListContainers(ctx context.Context, allProjects bool) ([]ContainerRow, error) {
	if d.baseURL == "" {
		return nil, fmt.Errorf("docker proxy base URL not configured (DOCKER_PROXY_URL)")
	}
	q := url.Values{}
	q.Set("all", "true")
	if !allProjects {
		// Docker's filters param is a JSON object. The label filter accepts a
		// list of "key=value" strings; a map of key -> [values] is rejected
		// with HTTP 400 (matches admin server.go handleDocker).
		q.Set("filters", `{"label":["com.docker.compose.project=pixeleruv"]}`)
	}
	u := d.baseURL + "/containers/json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker proxy: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("docker proxy read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker proxy: HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Docker's /containers/json shape. Names is a []string with leading "/".
	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		ImageID string            `json:"ImageID"`
		State   string            `json:"State"`
		Status  string            `json:"Status"`
		Created int64             `json:"Created"` // unix seconds
		Labels  map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("docker proxy decode: %w", err)
	}
	rows := make([]ContainerRow, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		rows = append(rows, ContainerRow{
			ID:      c.ID,
			Name:    name,
			Image:   c.Image,
			ImageID: c.ImageID,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
			Labels:  c.Labels,
		})
	}
	return rows, nil
}

// Info calls GET /info on the docker-readonly-proxy and returns the raw
// engine info JSON (containers/containersRunning/builds, etc.).
func (d *DockerClient) Info(ctx context.Context) (json.RawMessage, error) {
	if d.baseURL == "" {
		return nil, fmt.Errorf("docker proxy base URL not configured (DOCKER_PROXY_URL)")
	}
	u := d.baseURL + "/info"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker proxy: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("docker proxy read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker proxy: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}
