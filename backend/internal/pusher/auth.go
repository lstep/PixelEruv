package pusher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"crypto/rsa"

	"github.com/golang-jwt/jwt/v5"
)

// AuthValidator validates OIDC id_tokens against Dex's JWKS.
type AuthValidator struct {
	issuer   string
	clientID string
	jwksURL  string
	keys     map[string]*rsa.PublicKey
	mu       sync.RWMutex
}

type jwkKey struct {
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

func NewAuthValidator(dexURL, clientID string) *AuthValidator {
	return &AuthValidator{
		issuer:   dexURL,
		clientID: clientID,
		jwksURL:  dexURL + "/keys",
		keys:     make(map[string]*rsa.PublicKey),
	}
}

// fetchKeys fetches the JWKS from Dex and caches the RSA public keys.
func (a *AuthValidator) fetchKeys() error {
	resp, err := http.Get(a.jwksURL)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("jwks responded %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read jwks: %w", err)
	}

	var jwks jwksResponse
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parse jwks: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		key, err := rsaKeyFromJWK(k)
		if err != nil {
			continue
		}
		a.keys[k.Kid] = key
	}
	return nil
}

// rsaKeyFromJWK builds an rsa.PublicKey from the base64url-encoded n and e.
func rsaKeyFromJWK(k jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: e,
	}, nil
}

// startKeyRefresh refreshes the JWKS every 10 minutes.
func (a *AuthValidator) startKeyRefresh(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = a.fetchKeys()
		}
	}
}

// ValidateToken validates an id_token and returns the sub claim.
func (a *AuthValidator) ValidateToken(idToken string) (string, error) {
	token, err := jwt.Parse(idToken, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("unexpected alg: %v", t.Method.Alg())
		}
		kid, ok := t.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid in token header")
		}
		a.mu.RLock()
		key, exists := a.keys[kid]
		a.mu.RUnlock()
		if !exists {
			// Key might have rotated — try refreshing once.
			if err := a.fetchKeys(); err != nil {
				return nil, fmt.Errorf("key not found and refresh failed: %w", err)
			}
			a.mu.RLock()
			key, exists = a.keys[kid]
			a.mu.RUnlock()
			if !exists {
				return nil, fmt.Errorf("kid %s not in jwks", kid)
			}
		}
		return key, nil
	})
	if err != nil {
		return "", fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("invalid claims type")
	}

	// Verify issuer.
	iss, _ := claims["iss"].(string)
	if iss != a.issuer {
		return "", fmt.Errorf("invalid issuer: %s", iss)
	}

	// Verify audience.
	aud, _ := claims["aud"].(string)
	if aud != a.clientID {
		audList, ok := claims["aud"].([]interface{})
		if !ok {
			return "", fmt.Errorf("invalid audience: %v", claims["aud"])
		}
		found := false
		for _, v := range audList {
			if s, ok := v.(string); ok && s == a.clientID {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("audience does not include %s", a.clientID)
		}
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", fmt.Errorf("empty sub claim")
	}
	return sub, nil
}
