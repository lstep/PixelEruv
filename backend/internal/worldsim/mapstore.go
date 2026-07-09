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

// MapStore handles the maps PB collection: it can seed a default map record
// (Tiled JSON + tileset PNGs) on first run when no record exists for the
// configured map name. It authenticates as a superuser, mirroring SpriteStore.
type MapStore struct {
	pocketbaseURL string
	adminEmail    string
	adminPassword string
	token         string
	mu            sync.Mutex
}

func NewMapStore(pocketbaseURL, adminEmail, adminPassword string) *MapStore {
	return &MapStore{
		pocketbaseURL: strings.TrimRight(pocketbaseURL, "/"),
		adminEmail:    adminEmail,
		adminPassword: adminPassword,
	}
}

// authToken returns a cached superuser token, authenticating if needed.
func (s *MapStore) authToken() (string, error) {
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

func (s *MapStore) doRequest(method, url string, body io.Reader, contentType string) (*http.Response, error) {
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

// mapRecordExists returns true if a maps record with the given name exists.
func (s *MapStore) mapRecordExists(mapName string) (bool, error) {
	url := fmt.Sprintf("%s/api/collections/maps/records?filter=(name=\"%s\")&perPage=1", s.pocketbaseURL, mapName)
	resp, err := s.doRequest("GET", url, nil, "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("pocketbase %d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return len(result.Items) > 0, nil
}

// SeedMapIfMissing uploads the Tiled JSON and referenced tileset PNGs from
// mapDir as a new maps record named mapName, but only if no record with that
// name exists yet. Idempotent: no-op if the map is already present.
// jsonFile is the filename of the Tiled JSON inside mapDir (e.g.
// "default-map.json").
func (s *MapStore) SeedMapIfMissing(mapName, mapDir, jsonFile string) error {
	exists, err := s.mapRecordExists(mapName)
	if err != nil {
		return fmt.Errorf("check map record: %w", err)
	}
	if exists {
		return nil
	}

	jsonPath := filepath.Join(mapDir, jsonFile)
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("read map json %s: %w", jsonPath, err)
	}

	// Extract tileset image filenames from the Tiled JSON so we upload exactly
	// the referenced PNGs (PocketBase requires at least one tileset file).
	var tiled struct {
		Tilesets []struct {
			Image string `json:"image"`
		} `json:"tilesets"`
	}
	if err := json.Unmarshal(jsonBytes, &tiled); err != nil {
		return fmt.Errorf("parse map json: %w", err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("name", mapName); err != nil {
		return fmt.Errorf("write name field: %w", err)
	}
	// tiled_json file field.
	jfw, err := w.CreateFormFile("tiled_json", jsonFile)
	if err != nil {
		return fmt.Errorf("create tiled_json form file: %w", err)
	}
	if _, err := jfw.Write(jsonBytes); err != nil {
		return fmt.Errorf("write tiled_json: %w", err)
	}
	// tilesets file field — one part per referenced PNG.
	for _, ts := range tiled.Tilesets {
		pngName := ts.Image
		if pngName == "" {
			continue
		}
		// Tiled may store a relative path; use the basename.
		if base := filepath.Base(pngName); base != "" {
			pngName = base
		}
		pngPath := filepath.Join(mapDir, pngName)
		f, err := os.Open(pngPath)
		if err != nil {
			return fmt.Errorf("open tileset %s: %w", pngName, err)
		}
		fw, err := w.CreateFormFile("tilesets", pngName)
		if err != nil {
			f.Close()
			return fmt.Errorf("create tileset form file %s: %w", pngName, err)
		}
		if _, err := io.Copy(fw, f); err != nil {
			f.Close()
			return fmt.Errorf("copy tileset %s: %w", pngName, err)
		}
		f.Close()
	}
	w.Close()

	url := fmt.Sprintf("%s/api/collections/maps/records", s.pocketbaseURL)
	resp, err := s.doRequest("POST", url, &buf, w.FormDataContentType())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pocketbase create map %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
