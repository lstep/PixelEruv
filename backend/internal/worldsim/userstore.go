package worldsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// UserRecord represents a user in PocketBase's users collection.
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

// UserStore handles PocketBase user lookups and persistence.
type UserStore struct {
	pocketbaseURL string
}

func NewUserStore(pocketbaseURL string) *UserStore {
	return &UserStore{pocketbaseURL: strings.TrimRight(pocketbaseURL, "/")}
}

// FindOrCreateUser looks up a user by oidc_sub. If not found, creates a new
// record with a generated entity_id and default spawn position.
func (s *UserStore) FindOrCreateUser(sub, entityID string) (*UserRecord, error) {
	// Try to find existing user.
	user, err := s.findBySub(sub)
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	if user != nil {
		return user, nil
	}

	// Create new user.
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

// SavePosition updates the user's position in PocketBase.
func (s *UserStore) SavePosition(entityID string, x, y float32) error {
	// Find the record by entity_id to get its PocketBase id.
	user, err := s.findByEntityID(entityID)
	if err != nil {
		return fmt.Errorf("find user for save: %w", err)
	}
	if user == nil {
		return nil
	}

	body := fmt.Sprintf(`{"pos_x":%g,"pos_y":%g}`, x, y)
	url := fmt.Sprintf("%s/api/collections/users/records/%s", s.pocketbaseURL, user.ID)
	req, err := http.NewRequest("PATCH", url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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
	url := fmt.Sprintf("%s/api/collections/users/records?filter=(oidc_sub=\"%s\")&perPage=1", s.pocketbaseURL, sub)
	resp, err := http.Get(url)
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
	url := fmt.Sprintf("%s/api/collections/users/records?filter=(entity_id=\"%s\")&perPage=1", s.pocketbaseURL, entityID)
	resp, err := http.Get(url)
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
	url := fmt.Sprintf("%s/api/collections/users/records", s.pocketbaseURL)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
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
