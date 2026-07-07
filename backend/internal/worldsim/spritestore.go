package worldsim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// SpriteBase represents a record in PocketBase's sprite_bases collection.
type SpriteBase struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type spriteBaseListResponse struct {
	Items []SpriteBase `json:"items"`
}

// SpriteStore handles the sprite_bases PB collection: catalog reads for
// validation, and seeding on first run. It authenticates as a superuser to
// get a token for write operations, mirroring UserStore.
type SpriteStore struct {
	pocketbaseURL string
	adminEmail    string
	adminPassword string
	token         string
	mu            sync.Mutex
}

func NewSpriteStore(pocketbaseURL, adminEmail, adminPassword string) *SpriteStore {
	return &SpriteStore{
		pocketbaseURL: strings.TrimRight(pocketbaseURL, "/"),
		adminEmail:    adminEmail,
		adminPassword: adminPassword,
	}
}

// authToken returns a cached superuser token, authenticating if needed.
func (s *SpriteStore) authToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	body := fmt.Sprintf(`{"identity":"%s","password":"%s"}`, s.adminEmail, s.adminPassword)
	resp, err := http.Post(
		s.pocketbaseURL+"/api/collections/_superusers/auth-with-password",
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("pb auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pb auth %d: %s", resp.StatusCode, string(b))
	}
	var auth pbAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		return "", fmt.Errorf("pb auth decode: %w", err)
	}
	s.token = auth.Token
	return s.token, nil
}

// doRequest creates an authenticated request to PocketBase.
func (s *SpriteStore) doRequest(method, url string, body io.Reader, contentType string) (*http.Response, error) {
	token, err := s.authToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return http.DefaultClient.Do(req)
}

// ListBases returns all sprite_bases records.
func (s *SpriteStore) ListBases() ([]SpriteBase, error) {
	url := fmt.Sprintf("%s/api/collections/sprite_bases/records?perPage=200", s.pocketbaseURL)
	resp, err := s.doRequest("GET", url, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pocketbase %d: %s", resp.StatusCode, string(b))
	}
	var result spriteBaseListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// BaseExists checks if a sprite_bases record ID exists. Used by
// handleSetSpriteBase for validation.
func (s *SpriteStore) BaseExists(id string) (bool, error) {
	if id == "" {
		return true, nil // empty = fallback, always "valid"
	}
	url := fmt.Sprintf("%s/api/collections/sprite_bases/records/%s", s.pocketbaseURL, id)
	resp, err := s.doRequest("GET", url, nil, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400, nil
}

// SeedIfEmpty uploads every PNG in dir as a sprite_bases record, but only if
// the collection is currently empty. Idempotent: no-op if any records exist.
// Called once at worldsim startup.
func (s *SpriteStore) SeedIfEmpty(dir string) error {
	bases, err := s.ListBases()
	if err != nil {
		return fmt.Errorf("list sprite_bases: %w", err)
	}
	if len(bases) > 0 {
		return nil
	}
	return s.Seed(dir, false)
}

// Seed uploads PNGs from dir. With force=false it returns early if the
// collection is non-empty (equivalent to SeedIfEmpty). With force=true it
// uploads every PNG, skipping per-file if a record with that name already
// exists. Used by the cmd/seed-sprites CLI.
func (s *SpriteStore) Seed(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read sprites dir %s: %w", dir, err)
	}

	// Build a set of existing names for per-file skip (force mode).
	existing := map[string]bool{}
	if force {
		bases, err := s.ListBases()
		if err != nil {
			return fmt.Errorf("list sprite_bases: %w", err)
		}
		for _, b := range bases {
			existing[b.Name] = true
		}
	} else {
		bases, err := s.ListBases()
		if err != nil {
			return fmt.Errorf("list sprite_bases: %w", err)
		}
		if len(bases) > 0 {
			return nil
		}
	}

	seeded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.ToLower(filepath.Ext(name)) != ".png" {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if existing[stem] {
			continue
		}
		if err := s.uploadBase(filepath.Join(dir, name), stem); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		seeded++
	}
	if seeded > 0 {
		// Logged by caller; no log here to keep the store testable.
	}
	return nil
}

// uploadBase POSTs a single PNG as a new sprite_bases record with the given
// name, using multipart/form-data for the file field.
func (s *SpriteStore) uploadBase(path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("name", name); err != nil {
		return fmt.Errorf("write name field: %w", err)
	}
	fw, err := w.CreateFormFile("sheet", filepath.Base(path))
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}
	w.Close()

	url := fmt.Sprintf("%s/api/collections/sprite_bases/records", s.pocketbaseURL)
	resp, err := s.doRequest("POST", url, &buf, w.FormDataContentType())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pocketbase create %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
