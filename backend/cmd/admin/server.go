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
	"os"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/version"
	nats "github.com/nats-io/nats.go"
	"golang.org/x/sys/unix"
)

// Config holds the admin service configuration.
type Config struct {
	SessionSecret   string
	PBApiURL        string
	PBAdminEmail    string
	PBAdminPassword string
	RecordingsDir   string
	NATSURL         string
}

// Server is the admin portal HTTP service.
type Server struct {
	cfg    Config
	logger *slog.Logger
	tmpl   *template.Template
	nc     *nats.Conn
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Parse(landingHTML)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	if _, err := tmpl.New("recordings").Parse(recordingsHTML); err != nil {
		return nil, fmt.Errorf("parse recordings template: %w", err)
	}
	// NATS is optional: the stop button is disabled if not connected,
	// but other admin features (list, delete, delete-all) still work.
	var nc *nats.Conn
	if cfg.NATSURL != "" {
		nc, err = nats.Connect(cfg.NATSURL,
			nats.Name("pixeleruv-admin"),
			nats.ReconnectWait(2*time.Second),
			nats.MaxReconnects(-1),
		)
		if err != nil {
			logger.Warn("nats connect (stop button will be disabled)", "err", err, "url", cfg.NATSURL)
			nc = nil
		} else {
			logger.Info("nats connected", "url", cfg.NATSURL)
		}
	}
	return &Server{cfg: cfg, logger: logger, tmpl: tmpl, nc: nc}, nil
}

func (s *Server) Run(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/", s.handleLanding)
	mux.HandleFunc("/admin/login", s.handleLogin)
	mux.HandleFunc("/admin/authenticate", s.handleAuthenticate)
	mux.HandleFunc("/admin/logout", s.handleLogout)
	mux.HandleFunc("/admin/auth-check", s.handleAuthCheck)
	mux.HandleFunc("/admin/recordings", s.handleRecordings)
	mux.HandleFunc("/admin/recordings/delete", s.handleRecordingsDelete)
	mux.HandleFunc("/admin/recordings/delete-all", s.handleRecordingsDeleteAll)
	mux.HandleFunc("/admin/recordings/stop", s.handleRecordingsStop)

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

// pbAdminToken authenticates to PocketBase as superadmin and returns the
// auth token. Used to access admin-only collections (recordings).
func (s *Server) pbAdminToken() (string, error) {
	if s.cfg.PBAdminEmail == "" || s.cfg.PBAdminPassword == "" {
		return "", fmt.Errorf("PB_ADMIN_EMAIL/PB_ADMIN_PASSWORD not configured")
	}
	body, _ := json.Marshal(map[string]string{
		"identity": s.cfg.PBAdminEmail,
		"password": s.cfg.PBAdminPassword,
	})
	resp, err := http.Post(
		s.cfg.PBApiURL+"/collections/_superusers/auth-with-password",
		"application/json",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return "", fmt.Errorf("pb admin auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("pb admin auth: status %d", resp.StatusCode)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("pb admin auth: decode: %w", err)
	}
	return result.Token, nil
}

// recordingRow is one row in the recordings table.
type recordingRow struct {
	ID           string
	MeetingID    string
	Room         string
	ZoneID       string
	Target       string
	Status       string
	StartedBy    string
	StartTime    string
	EndTime      string
	Duration     string
	Participants string
	FileURL      string
	HasFile      bool
	AudioURL     string
	HasAudio     bool
	AudioStatus  string // ""|pending|ok|failed
	AudioError   string
}

// handleRecordings renders the recordings management page with optional
// search filters (room, status, target, started_by).
func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.getSession(r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}

	q := r.URL.Query()
	room := q.Get("room")
	status := q.Get("status")
	target := q.Get("target")
	startedBy := q.Get("started_by")

	token, err := s.pbAdminToken()
	if err != nil {
		s.logger.Warn("pb admin token", "err", err)
		http.Error(w, "failed to authenticate to PocketBase", http.StatusBadGateway)
		return
	}

	// Build PB query with filters.
	pbURL := s.cfg.PBApiURL + "/collections/recordings/records?perPage=100&sort=-start_time"
	var filters []string
	if room != "" {
		filters = append(filters, fmt.Sprintf("room~%q", room))
	}
	if status != "" {
		filters = append(filters, fmt.Sprintf("status=%q", status))
	}
	if target != "" {
		filters = append(filters, fmt.Sprintf("target=%q", target))
	}
	if startedBy != "" {
		filters = append(filters, fmt.Sprintf("started_by~%q", startedBy))
	}
	if len(filters) > 0 {
		pbURL += "&filter=" + url.QueryEscape(strings.Join(filters, " && "))
	}

	req, _ := http.NewRequest("GET", pbURL, nil)
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn("pb recordings query", "err", err)
		http.Error(w, "failed to query recordings", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		s.logger.Warn("pb recordings query status", "status", resp.StatusCode)
		http.Error(w, "failed to query recordings", http.StatusBadGateway)
		return
	}

	var result struct {
		Items []struct {
			ID           string   `json:"id"`
			MeetingID    string   `json:"meeting_id"`
			Room         string   `json:"room"`
			ZoneID       string   `json:"zone_id"`
			Target       string   `json:"target"`
			Status       string   `json:"status"`
			StartedBy    string   `json:"started_by"`
			StartTime    string   `json:"start_time"`
			EndTime      string   `json:"end_time"`
			Participants []string `json:"participants"`
			FileURL      string   `json:"file_url"`
			AudioURL     string   `json:"audio_url"`
			AudioStatus  string   `json:"audio_status"`
			AudioError   string   `json:"audio_error"`
		} `json:"items"`
		TotalItems int `json:"totalItems"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.logger.Warn("pb recordings decode", "err", err)
		http.Error(w, "failed to parse recordings", http.StatusBadGateway)
		return
	}

	rows := make([]recordingRow, 0, len(result.Items))
	for _, item := range result.Items {
		row := recordingRow{
			ID:           item.ID,
			MeetingID:    item.MeetingID,
			Room:         item.Room,
			ZoneID:       item.ZoneID,
			Target:       item.Target,
			Status:       item.Status,
			StartedBy:    item.StartedBy,
			StartTime:    item.StartTime,
			EndTime:      item.EndTime,
			Participants: strings.Join(item.Participants, ", "),
			FileURL:      item.FileURL,
			HasFile:      item.FileURL != "",
			AudioURL:     item.AudioURL,
			HasAudio:     item.AudioURL != "",
			AudioStatus:  item.AudioStatus,
			AudioError:   item.AudioError,
		}
		row.Duration = computeDuration(item.StartTime, item.EndTime)
		rows = append(rows, row)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.ExecuteTemplate(w, "recordings", map[string]any{
		"Email":     sess.Email,
		"Version":   version.Version,
		"Rows":      rows,
		"Total":     result.TotalItems,
		"Room":      room,
		"Status":    status,
		"Target":    target,
		"StartedBy": startedBy,
		"Disk":      diskUsage(s.cfg.RecordingsDir),
	})
}

// computeDuration returns a human-readable duration between start and end
// times. PocketBase serializes DateField as "2006-01-02 15:04:05.000Z"
// (space-separated, not RFC3339's T separator). Returns "" if end is empty.
func computeDuration(start, end string) string {
	if start == "" || end == "" {
		return ""
	}
	t1, err1 := time.Parse("2006-01-02 15:04:05.000Z", start)
	t2, err2 := time.Parse("2006-01-02 15:04:05.000Z", end)
	if err1 != nil || err2 != nil {
		return ""
	}
	d := t2.Sub(t1)
	if d < 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// diskUsage reports free/total bytes and free percentage for the filesystem
// holding dir. Returns zero values on error (e.g. dir doesn't exist).
type diskInfo struct {
	TotalBytes  uint64
	FreeBytes   uint64
	UsedBytes   uint64
	FreePercent float64
	TotalHuman  string
	FreeHuman   string
	UsedHuman   string
}

func diskUsage(dir string) diskInfo {
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return diskInfo{}
	}
	total := uint64(fs.Blocks) * uint64(fs.Bsize)
	free := uint64(fs.Bavail) * uint64(fs.Bsize)
	used := total - free
	pct := 0.0
	if total > 0 {
		pct = float64(free) / float64(total) * 100
	}
	return diskInfo{
		TotalBytes:  total,
		FreeBytes:   free,
		UsedBytes:   used,
		FreePercent: pct,
		TotalHuman:  humanBytes(total),
		FreeHuman:   humanBytes(free),
		UsedHuman:   humanBytes(used),
	}
}

// humanBytes formats a byte count as a human-readable string (e.g. "1.2 GB").
func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// handleRecordingsDelete deletes a recording: PB record + file on disk.
func (s *Server) handleRecordingsDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.getSession(r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.FormValue("id")
	fileURL := r.FormValue("file_url")
	audioURL := r.FormValue("audio_url")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	token, err := s.pbAdminToken()
	if err != nil {
		s.logger.Warn("pb admin token", "err", err)
		http.Error(w, "failed to authenticate to PocketBase", http.StatusBadGateway)
		return
	}

	// Delete the PB record.
	delURL := fmt.Sprintf("%s/collections/recordings/records/%s", s.cfg.PBApiURL, id)
	req, _ := http.NewRequest("DELETE", delURL, nil)
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn("pb recording delete", "err", err, "id", id)
		http.Error(w, "failed to delete record", http.StatusBadGateway)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		s.logger.Warn("pb recording delete status", "status", resp.StatusCode, "id", id)
		http.Error(w, "failed to delete record", http.StatusBadGateway)
		return
	}

	// Delete files from disk if URLs point to local /recordings/ paths.
	if s.cfg.RecordingsDir != "" {
		for _, u := range []string{fileURL, audioURL} {
			if u == "" {
				continue
			}
			if filename := extractFilename(u); filename != "" {
				path := s.cfg.RecordingsDir + "/" + filename
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					s.logger.Warn("delete recording file", "err", err, "path", path)
					// Non-fatal: the PB record is already deleted.
				}
			}
		}
	}

	s.logger.Info("recording deleted", "id", id, "by", sess.Email, "file_url", fileURL, "audio_url", audioURL)
	http.Redirect(w, r, "/admin/recordings", http.StatusFound)
}

// handleRecordingsDeleteAll wipes all recordings: every PB record and every
// file in the recordings directory. The PB records are deleted via the
// superadmin token; the files are removed by clearing the recordings dir.
func (s *Server) handleRecordingsDeleteAll(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.getSession(r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	confirm := r.FormValue("confirm")
	if confirm != "DELETE ALL" {
		http.Error(w, "confirmation missing or incorrect", http.StatusBadRequest)
		return
	}

	token, err := s.pbAdminToken()
	if err != nil {
		s.logger.Warn("pb admin token", "err", err)
		http.Error(w, "failed to authenticate to PocketBase", http.StatusBadGateway)
		return
	}

	// Fetch all recording records (paginate in case of >100).
	var allIDs []string
	page := 1
	for {
		u := fmt.Sprintf("%s/collections/recordings/records?perPage=100&page=%d", s.cfg.PBApiURL, page)
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			s.logger.Warn("pb recordings list", "err", err, "page", page)
			http.Error(w, "failed to list recordings", http.StatusBadGateway)
			return
		}
		var result struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			TotalPages int `json:"totalPages"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			s.logger.Warn("pb recordings decode", "err", err)
			http.Error(w, "failed to parse recordings", http.StatusBadGateway)
			return
		}
		for _, it := range result.Items {
			allIDs = append(allIDs, it.ID)
		}
		if page >= result.TotalPages || len(result.Items) == 0 {
			break
		}
		page++
	}

	// Delete each PB record.
	deleted := 0
	for _, id := range allIDs {
		u := fmt.Sprintf("%s/collections/recordings/records/%s", s.cfg.PBApiURL, id)
		req, _ := http.NewRequest("DELETE", u, nil)
		req.Header.Set("Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			s.logger.Warn("pb recording delete", "err", err, "id", id)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 204 {
			deleted++
		}
	}

	// Wipe the recordings directory contents (files only, keep the dir).
	filesRemoved := 0
	if s.cfg.RecordingsDir != "" {
		entries, err := os.ReadDir(s.cfg.RecordingsDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if err := os.Remove(s.cfg.RecordingsDir + "/" + e.Name()); err != nil {
					s.logger.Warn("delete recording file", "err", err, "file", e.Name())
				} else {
					filesRemoved++
				}
			}
		} else {
			s.logger.Warn("read recordings dir", "err", err)
		}
	}

	s.logger.Info("all recordings deleted", "by", sess.Email, "records", deleted, "files", filesRemoved)
	http.Redirect(w, r, "/admin/recordings", http.StatusFound)
}

// handleRecordingsStop stops a currently-active recording by publishing
// a recording.admin.stop message to ext-rec via NATS. ext-rec performs
// the clean stop flow (StopEgress, PB update, audio extraction).
func (s *Server) handleRecordingsStop(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.getSession(r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	room := r.FormValue("room")
	meetingID := r.FormValue("meeting_id")
	if room == "" || meetingID == "" {
		http.Error(w, "missing room or meeting_id", http.StatusBadRequest)
		return
	}
	if s.nc == nil {
		s.logger.Warn("stop requested but NATS not connected", "room", room)
		http.Error(w, "NATS unavailable; admin service has no bus connection", http.StatusServiceUnavailable)
		return
	}

	// Fetch the PB record to confirm it is actually active before sending.
	token, err := s.pbAdminToken()
	if err != nil {
		s.logger.Warn("pb admin token", "err", err)
		http.Error(w, "failed to authenticate to PocketBase", http.StatusBadGateway)
		return
	}
	pbURL := fmt.Sprintf("%s/collections/recordings/records?filter=%s",
		s.cfg.PBApiURL,
		url.QueryEscape(fmt.Sprintf("meeting_id=%q", meetingID)),
	)
	req, _ := http.NewRequest("GET", pbURL, nil)
	req.Header.Set("Authorization", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn("pb recordings query", "err", err)
		http.Error(w, "failed to query recording", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	var result struct {
		Items []struct {
			Status string `json:"status"`
			Room   string `json:"room"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.logger.Warn("pb recordings decode", "err", err)
		http.Error(w, "failed to parse recording", http.StatusBadGateway)
		return
	}
	if len(result.Items) == 0 {
		http.Error(w, "recording not found", http.StatusNotFound)
		return
	}
	if result.Items[0].Status != "active" {
		http.Error(w, "recording is not active (status="+result.Items[0].Status+")", http.StatusConflict)
		return
	}

	// Publish to ext-rec. Fire-and-forget: ext-rec handles the full stop
	// flow and updates the PB row. The admin UI re-queries on next page
	// load, so no reply is needed.
	msg := struct {
		Room       string `json:"room"`
		MeetingID  string `json:"meeting_id"`
		AdminEmail string `json:"admin_email"`
	}{Room: room, MeetingID: meetingID, AdminEmail: sess.Email}
	data, _ := json.Marshal(msg)
	if err := s.nc.Publish("recording.admin.stop", data); err != nil {
		s.logger.Warn("nats publish recording.admin.stop", "err", err)
		http.Error(w, "failed to publish stop request", http.StatusBadGateway)
		return
	}
	s.logger.Info("stop request published", "room", room, "meeting", meetingID, "by", sess.Email)
	http.Redirect(w, r, "/admin/recordings", http.StatusFound)
}

// extractFilename pulls the filename out of a file_url like
// https://host/recordings/zone-room-123.mp4. Returns "" if the URL
// doesn't match the expected pattern.
func extractFilename(fileURL string) string {
	idx := strings.Index(fileURL, "/recordings/")
	if idx < 0 {
		return ""
	}
	return fileURL[idx+len("/recordings/"):]
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
