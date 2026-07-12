package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/version"
)

// Config holds the admin service configuration.
type Config struct {
	SessionSecret string
	PBApiURL      string
}

// Server is the admin portal HTTP service.
type Server struct {
	cfg    Config
	logger *slog.Logger
	tmpl   *template.Template
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Parse(landingHTML)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	return &Server{cfg: cfg, logger: logger, tmpl: tmpl}, nil
}

func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/", s.handleLanding)
	mux.HandleFunc("/admin/login", s.handleLogin)
	mux.HandleFunc("/admin/authenticate", s.handleAuthenticate)
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

// handleLogin renders the email/password login form.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// If already logged in, redirect to landing.
	if _, ok := s.getSession(r); ok {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, loginHTML)
}

// handleAuthenticate handles the POST from the login form. It calls
// PocketBase's auth-with-password endpoint to validate credentials,
// then checks the is_admin flag on the players collection.
func (s *Server) handleAuthenticate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	email := r.FormValue("email")
	password := r.FormValue("password")
	if email == "" || password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}

	// Call PocketBase auth-with-password.
	form := url.Values{
		"identity": {email},
		"password": {password},
	}
	resp, err := http.Post(
		s.cfg.PBApiURL+"/collections/users/auth-with-password",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		s.logger.Warn("pb auth call", "err", err)
		http.Error(w, "authentication failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		s.logger.Warn("pb auth rejected", "status", resp.StatusCode)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	var authResult struct {
		Record struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"record"`
	}
	if err := json.Unmarshal(body, &authResult); err != nil {
		s.logger.Warn("parse auth result", "err", err)
		http.Error(w, "authentication failed", http.StatusBadGateway)
		return
	}

	sub := authResult.Record.ID
	emailOut := authResult.Record.Email

	// Check is_admin via PB API.
	isAdmin, err := s.checkIsAdmin(sub)
	if err != nil {
		s.logger.Warn("is_admin check", "err", err, "sub", sub)
		http.Error(w, "failed to verify admin status", http.StatusForbidden)
		return
	}
	if !isAdmin {
		s.logger.Info("non-admin login attempt", "sub", sub, "email", emailOut)
		http.Error(w, "not an admin user", http.StatusForbidden)
		return
	}

	// Set session cookie.
	s.setSessionCookie(w, sessionCookie{
		Sub:    sub,
		Email:  emailOut,
		Expiry: time.Now().Add(time.Hour).Unix(),
	})

	s.logger.Info("admin login", "sub", sub, "email", emailOut)
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
	// Query PB players collection filtered by user_id.
	filter := fmt.Sprintf("user_id=%q", sub)
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

// loginHTML is the email/password login form for the admin portal.
const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Admin Login - PixelEruv</title>
<style>
:root {
  --bg: #1a1a2e;
  --surface: #16213e;
  --text: #e0e0e0;
  --muted: #888;
  --accent: #4e9af1;
  --error: #e74c3c;
}
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: system-ui, sans-serif; background: var(--bg); color: var(--text); min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 2rem; }
.card { background: var(--surface); border-radius: 12px; padding: 2rem; max-width: 400px; width: 100%; }
h1 { font-size: 1.5rem; margin-bottom: 0.5rem; }
p.subtitle { color: var(--muted); font-size: 0.9rem; margin-bottom: 1.5rem; }
label { display: block; color: var(--muted); font-size: 0.85rem; margin-bottom: 0.3rem; margin-top: 1rem; }
input { width: 100%; padding: 0.6rem 0.8rem; font-size: 0.95rem; background: var(--bg); color: var(--text); border: 1px solid #444; border-radius: 6px; }
input:focus { outline: none; border-color: var(--accent); }
button { width: 100%; padding: 0.7rem; margin-top: 1.5rem; font-size: 1rem; font-weight: 600; background: var(--accent); color: white; border: none; border-radius: 6px; cursor: pointer; }
button:hover { opacity: 0.9; }
.back { display: block; margin-top: 1.5rem; text-align: center; color: var(--muted); text-decoration: none; font-size: 0.85rem; }
.back:hover { color: var(--text); }
</style>
</head>
<body>
<div class="card">
  <h1>Admin Login</h1>
  <p class="subtitle">Sign in with your PixelEruv account.</p>
  <form method="POST" action="/admin/authenticate">
    <label for="email">Email</label>
    <input type="email" id="email" name="email" required autofocus>
    <label for="password">Password</label>
    <input type="password" id="password" name="password" required>
    <button type="submit">Log in</button>
  </form>
  <a class="back" href="/">← Back to PixelEruv</a>
</div>
</body>
</html>`
