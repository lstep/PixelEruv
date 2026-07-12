package pusher

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AuthValidator validates PocketBase auth tokens by calling the
// PocketBase API. PocketBase JWTs are signed with a per-user token key
// combined with the collection's AuthToken secret, so local validation
// would require DB access. Instead, we make one HTTP call per WebSocket
// connection to PocketBase's API, which validates the token server-side.
type AuthValidator struct {
	pbAPIURL string
	client   *http.Client
}

type jwtpayload struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func NewAuthValidator(pbAPIURL string) *AuthValidator {
	return &AuthValidator{
		pbAPIURL: strings.TrimRight(pbAPIURL, "/"),
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// extractIDFromJWT decodes the JWT payload (without signature verification)
// to get the "id" claim. This is used to construct the PocketBase API URL
// for token validation — the actual signature validation happens server-side.
func extractIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid JWT format")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload jwtpayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return "", fmt.Errorf("parse JWT payload: %w", err)
	}
	if payload.ID == "" {
		return "", fmt.Errorf("empty id claim in token")
	}
	return payload.ID, nil
}

// ValidateToken validates a PocketBase auth token by calling the PocketBase
// API. Returns the user record ID (used as user_id throughout the system).
func (a *AuthValidator) ValidateToken(idToken string) (string, error) {
	userID, err := extractIDFromJWT(idToken)
	if err != nil {
		return "", fmt.Errorf("extract id from token: %w", err)
	}

	// Call the PocketBase API with the token as Bearer auth. PocketBase
	// validates the JWT signature and expiration server-side. A 200 response
	// means the token is valid.
	url := fmt.Sprintf("%s/collections/users/records/%s", a.pbAPIURL, userID)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+idToken)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pocketbase api call: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pocketbase rejected token: status %d", resp.StatusCode)
	}

	return userID, nil
}
