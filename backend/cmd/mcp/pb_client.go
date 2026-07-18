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
)

// PocketBaseClient is a minimal REST client for PocketBase's collection
// records API. It uses an admin token for authentication. Used by the MCP
// server to expose player / map / sprite_base / ban record reads.
type PocketBaseClient struct {
	baseURL    string // e.g. "http://pocketbase:8090"
	adminToken string // optional; if empty, requests are unauthenticated
	httpClient *http.Client
}

func NewPocketBaseClient(baseURL, adminToken string) *PocketBaseClient {
	return &PocketBaseClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		adminToken: adminToken,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// PBRecord is a generic PocketBase record: id + arbitrary fields.
type PBRecord struct {
	ID         string                 `json:"id"`
	Collection string                 `json:"collection,omitempty"`
	Fields     map[string]any         `json:"fields,omitempty"`
}

// ListParams controls /records list queries (page/perPage/filter/sort).
type ListParams struct {
	PerPage int
	Page    int
	Filter  string // PocketBase filter expression, e.g. "is_default = true"
	Sort    string // e.g. "-created"
}

func (p *ListParams) toURLValues() url.Values {
	v := url.Values{}
	if p.PerPage > 0 {
		v.Set("perPage", strconv.Itoa(p.PerPage))
	}
	if p.Page > 0 {
		v.Set("page", strconv.Itoa(p.Page))
	}
	if p.Filter != "" {
		v.Set("filter", p.Filter)
	}
	if p.Sort != "" {
		v.Set("sort", p.Sort)
	}
	return v
}

// listResponse is the PocketBase paginated response shape.
type listResponse struct {
	Page       int             `json:"page"`
	PerPage    int             `json:"perPage"`
	TotalItems int             `json:"totalItems"`
	TotalPages int             `json:"totalPages"`
	Items      []map[string]any `json:"items"`
}

func (p *PocketBaseClient) do(ctx context.Context, method, path string) ([]byte, int, error) {
	if p.baseURL == "" {
		return nil, 0, fmt.Errorf("pocketbase base URL not configured (PB_BASE_URL)")
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if p.adminToken != "" {
		req.Header.Set("Authorization", p.adminToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("pocketbase %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("pocketbase read %s: %w", path, err)
	}
	return body, resp.StatusCode, nil
}

// ListRecords returns a page of records from a collection. The raw PocketBase
// response (including pagination metadata) is returned as-is so the MCP
// client can see totalItems / totalPages.
func (p *PocketBaseClient) ListRecords(ctx context.Context, collection string, params ListParams) (json.RawMessage, error) {
	if collection == "" {
		return nil, fmt.Errorf("collection is required")
	}
	path := "/api/collections/" + url.PathEscape(collection) + "/records?" + params.toURLValues().Encode()
	body, code, err := p.do(ctx, "GET", path)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("pocketbase list %s: HTTP %d: %s", collection, code, string(body))
	}
	return body, nil
}

// GetRecord returns a single record by ID.
func (p *PocketBaseClient) GetRecord(ctx context.Context, collection, id string) (json.RawMessage, error) {
	if collection == "" || id == "" {
		return nil, fmt.Errorf("collection and id are required")
	}
	path := "/api/collections/" + url.PathEscape(collection) + "/records/" + url.PathEscape(id)
	body, code, err := p.do(ctx, "GET", path)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("pocketbase get %s/%s: HTTP %d: %s", collection, id, code, string(body))
	}
	return body, nil
}
