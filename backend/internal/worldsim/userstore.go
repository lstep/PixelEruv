package worldsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// UserRecord represents a player in PocketBase's players collection.
type UserRecord struct {
	ID          string  `json:"id"`
	OidcSub     string  `json:"oidc_sub"`
	EntityID    string  `json:"entity_id"`
	DisplayName string  `json:"display_name"`
	PosX        float32 `json:"pos_x"`
	PosY        float32 `json:"pos_y"`
}

type pbListResponse struct {
	Items []UserRecord `json:"items"`
}

type pbAuthResponse struct {
	Token string `json:"token"`
}

// UserStore handles PocketBase player lookups and persistence.
// It authenticates as a superuser to get a token for write operations.
type UserStore struct {
	pocketbaseURL string
	adminEmail    string
	adminPassword string
	token         string
	mu            sync.Mutex
}

func NewUserStore(pocketbaseURL, adminEmail, adminPassword string) *UserStore {
	return &UserStore{
		pocketbaseURL: strings.TrimRight(pocketbaseURL, "/"),
		adminEmail:    adminEmail,
		adminPassword: adminPassword,
	}
}

// authToken returns a cached superuser token, authenticating if needed.
func (s *UserStore) authToken() (string, error) {
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
func (s *UserStore) doRequest(method, url string, body io.Reader) (*http.Response, error) {
	token, err := s.authToken()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

// FindOrCreateUser looks up a player by oidc_sub. If not found, creates a new
// record with a generated entity_id and default spawn position.
func (s *UserStore) FindOrCreateUser(sub, entityID string) (*UserRecord, error) {
	user, err := s.findBySub(sub)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user != nil {
		return user, nil
	}

	user = &UserRecord{
		OidcSub:  sub,
		EntityID: entityID,
		PosX:     10,
		PosY:     10,
	}
	if err := s.create(user); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// SavePosition updates the player's position in PocketBase.
func (s *UserStore) SavePosition(entityID string, x, y float32) error {
	user, err := s.findByEntityID(entityID)
	if err != nil {
		return fmt.Errorf("find user for save: %w", err)
	}
	if user == nil {
		return nil
	}

	body := fmt.Sprintf(`{"pos_x":%g,"pos_y":%g}`, x, y)
	url := fmt.Sprintf("%s/api/collections/players/records/%s", s.pocketbaseURL, user.ID)
	resp, err := s.doRequest("PATCH", url, strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pocketbase update %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (s *UserStore) findBySub(sub string) (*UserRecord, error) {
	url := fmt.Sprintf("%s/api/collections/players/records?filter=(oidc_sub=\"%s\")&perPage=1", s.pocketbaseURL, sub)
	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pocketbase %d: %s", resp.StatusCode, string(b))
	}
	var result pbListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}

func (s *UserStore) findByEntityID(entityID string) (*UserRecord, error) {
	url := fmt.Sprintf("%s/api/collections/players/records?filter=(entity_id=\"%s\")&perPage=1", s.pocketbaseURL, entityID)
	resp, err := s.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pocketbase %d: %s", resp.StatusCode, string(b))
	}
	var result pbListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}

func (s *UserStore) create(user *UserRecord) error {
	body := fmt.Sprintf(`{"oidc_sub":"%s","entity_id":"%s","pos_x":%g,"pos_y":%g}`,
		user.OidcSub, user.EntityID, user.PosX, user.PosY)
	url := fmt.Sprintf("%s/api/collections/players/records", s.pocketbaseURL)
	resp, err := s.doRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pocketbase create %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(user)
}
