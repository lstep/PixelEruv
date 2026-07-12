package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lstep/pixeleruv/backend/internal/version"
)

// Config holds the admin service configuration.
type Config struct {
	SessionSecret  string
	DexIssuer      string // issuer claim in JWT (must match Dex config issuer)
	DexInternalURL string // internal Docker URL for JWKS fetch + token exchange
	DexBrowserURL  string // browser-facing Dex URL for login redirects (e.g. /dex)
	DexClientID    string
	DexRedirectURL string
	PBApiURL       string
}

// Server is the admin portal HTTP service.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	validator *jwtValidator
	tmpl     *template.Template
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Parse(landingHTML)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	s := &Server{
		cfg:       cfg,
		logger:    logger,
		validator: newJWTValidator(cfg.DexIssuer, cfg.DexInternalURL, cfg.DexClientID),
		tmpl:      tmpl,
	}
	return s, nil
}

func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/", s.handleLanding)
	mux.HandleFunc("/admin/login", s.handleLogin)
	mux.HandleFunc("/admin/callback", s.handleCallback)
	mux.HandleFunc("/admin/logout", s.handleLogout)
	mux.HandleFunc("/admin/auth-check", s.handleAuthCheck)

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	return srv.ListenAndServe()
}

// --- Session cookie ---

// sessionCookie is the payload stored in the signed cookie.
type sessionCookie struct {
	Sub    string `json:"sub"`
	Email  string `json:"email"`
	Expiry int64  `json:"exp"` // unix timestamp
}

func (s *Server) setSessionCookie(w http.ResponseWriter, c sessionCookie) {
	data, _ := json.Marshal(c)
	encoded := base64.RawURLEncoding.EncodeToString(data)
	sig := s.signCookie(encoded)
	value := encoded + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    value,
		Path:     "/",
		MaxAge:   3600, // 1 hour
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) signCookie(encoded string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.SessionSecret))
	mac.Write([]byte(encoded))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyCookie(value string) (sessionCookie, bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return sessionCookie{}, false
	}
	encoded, sig := parts[0], parts[1]
	expectedSig := s.signCookie(encoded)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return sessionCookie{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return sessionCookie{}, false
	}
	var c sessionCookie
	if err := json.Unmarshal(data, &c); err != nil {
		return sessionCookie{}, false
	}
	if time.Now().Unix() > c.Expiry {
		return sessionCookie{}, false
	}
	return c, true
}

func (s *Server) getSession(r *http.Request) (sessionCookie, bool) {
	cookie, err := r.Cookie("admin_session")
	if err != nil {
		return sessionCookie{}, false
	}
	return s.verifyCookie(cookie.Value)
}

// --- PKCE helpers ---

func generateCodeVerifier() string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	b := make([]byte, 64)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		b[i] = chars[n.Int64()]
	}
	return string(b)
}

func computeCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randomState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- Handlers ---

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" && r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.getSession(r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.Execute(w, map[string]any{
		"Email":   sess.Email,
		"Version": version.Version,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	verifier := generateCodeVerifier()
	challenge := computeCodeChallenge(verifier)
	state := randomState()

	// Store verifier and state in short-lived cookies.
	http.SetCookie(w, &http.Cookie{
		Name: "pkce_verifier", Value: verifier, Path: "/",
		MaxAge: 300, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: "oauth_state", Value: state, Path: "/",
		MaxAge: 300, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})

	params := url.Values{
		"client_id":             {s.cfg.DexClientID},
		"redirect_uri":          {s.cfg.DexRedirectURL},
		"response_type":         {"code"},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, s.cfg.DexBrowserURL+"/auth?"+params.Encode(), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}

	// Verify state.
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != state {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	verifierCookie, err := r.Cookie("pkce_verifier")
	if err != nil {
		http.Error(w, "missing verifier", http.StatusBadRequest)
		return
	}

	// Clear PKCE cookies.
	http.SetCookie(w, &http.Cookie{Name: "pkce_verifier", MaxAge: -1, Path: "/", Secure: true})
	http.SetCookie(w, &http.Cookie{Name: "oauth_state", MaxAge: -1, Path: "/", Secure: true})

	// Exchange code for id_token.
	tokenParams := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.DexRedirectURL},
		"client_id":     {s.cfg.DexClientID},
		"code_verifier": {verifierCookie.Value},
	}
	resp, err := http.Post(s.cfg.DexInternalURL+"/token", "application/x-www-form-urlencoded", strings.NewReader(tokenParams.Encode()))
	if err != nil {
		s.logger.Warn("token exchange", "err", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		s.logger.Warn("token exchange status", "code", resp.StatusCode, "body", string(body))
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil || tokens.IDToken == "" {
		s.logger.Warn("no id_token in response", "err", err)
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}

	// Validate the JWT.
	sub, email, err := s.validator.validate(tokens.IDToken)
	if err != nil {
		s.logger.Warn("jwt validation", "err", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// Check is_admin via PB API.
	isAdmin, err := s.checkIsAdmin(sub)
	if err != nil {
		s.logger.Warn("is_admin check", "err", err, "sub", sub)
		http.Error(w, "failed to verify admin status", http.StatusForbidden)
		return
	}
	if !isAdmin {
		s.logger.Info("non-admin login attempt", "sub", sub, "email", email)
		http.Error(w, "not an admin user", http.StatusForbidden)
		return
	}

	// Set session cookie.
	s.setSessionCookie(w, sessionCookie{
		Sub:    sub,
		Email:  email,
		Expiry: time.Now().Add(time.Hour).Unix(),
	})

	s.logger.Info("admin login", "sub", sub, "email", email)
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getSession(r)
	if !ok {
		w.Header().Set("X-Redirect", "/admin/login")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// checkIsAdmin queries PocketBase for the user's is_admin flag.
func (s *Server) checkIsAdmin(sub string) (bool, error) {
	if sub == "" || sub == "dev" {
		return false, nil
	}
	// Query PB players collection filtered by oidc_sub.
	// PB filter syntax: oidc_sub="value" — quotes must be URL-encoded as %22.
	filter := fmt.Sprintf("oidc_sub=%q", sub)
	u := fmt.Sprintf("%s/collections/players/records?filter=%s", s.cfg.PBApiURL, url.QueryEscape(filter))
	resp, err := http.Get(u)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("pb responded %d", resp.StatusCode)
	}
	var result struct {
		Items []struct {
			IsAdmin bool `json:"is_admin"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	if len(result.Items) == 0 {
		return false, nil
	}
	return result.Items[0].IsAdmin, nil
}

// --- JWT validation (simplified from pusher/auth.go) ---

type jwtValidator struct {
	issuer     string
	internalURL string
	clientID   string
	keys       map[string]interface{} // kid -> public key
}

func newJWTValidator(issuer, internalURL, clientID string) *jwtValidator {
	return &jwtValidator{
		issuer:     issuer,
		internalURL: internalURL,
		clientID:   clientID,
		keys:       make(map[string]interface{}),
	}
}

func (v *jwtValidator) fetchKeys() error {
	resp, err := http.Get(v.internalURL + "/keys")
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
			Kty string `json:"kty"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parse jwks: %w", err)
	}

	v.keys = make(map[string]interface{})
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		key, err := rsaKeyFromJWK(k.Kid, k.N, k.E)
		if err != nil {
			continue
		}
		v.keys[k.Kid] = key
	}
	return nil
}

func (v *jwtValidator) validate(idToken string) (sub, email string, err error) {
	token, err := jwt.Parse(idToken, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != "RS256" {
			return nil, fmt.Errorf("unexpected alg: %v", t.Method.Alg())
		}
		kid, ok := t.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid")
		}
		key, exists := v.keys[kid]
		if !exists {
			if err := v.fetchKeys(); err != nil {
				return nil, err
			}
			key, exists = v.keys[kid]
			if !exists {
				return nil, fmt.Errorf("kid %s not found", kid)
			}
		}
		return key, nil
	})
	if err != nil {
		return "", "", err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", fmt.Errorf("invalid claims")
	}

	iss, _ := claims["iss"].(string)
	if iss != v.issuer {
		return "", "", fmt.Errorf("invalid issuer: %s", iss)
	}

	// Check audience (may be string or array).
	aud, _ := claims["aud"].(string)
	if aud != v.clientID {
		if audList, ok := claims["aud"].([]interface{}); ok {
			found := false
			for _, a := range audList {
				if s, ok := a.(string); ok && s == v.clientID {
					found = true
					break
				}
			}
			if !found {
				return "", "", fmt.Errorf("audience mismatch")
			}
		} else {
			return "", "", fmt.Errorf("audience mismatch")
		}
	}

	sub, _ = claims["sub"].(string)
	email, _ = claims["email"].(string)
	return sub, email, nil
}

// rsaKeyFromJWK builds an RSA public key from base64url-encoded n and e.
func rsaKeyFromJWK(kid, nStr, eStr string) (interface{}, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, err
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
