package worldsim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// WorldOptions is the full set of server-wide runtime options. The NATS KV
// bucket "world_options" (key "current") is the single source of truth; this
// struct is the in-memory representation used by worldsim and broadcast to
// consumers (ext-rec, frontend) via the "world_options.update" NATS subject.
//
// Fields marked readOnly are mirrored from env vars at startup and not
// editable from the admin UI (changing them at runtime would not reissue the
// TLS cert or re-mint LiveKit tokens). They are included so consumers and the
// admin UI can display them.
type WorldOptions struct {
	// SMTP for outgoing verification/password-reset emails.
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `json:"smtp_username"`
	SMTPPassword string `json:"smtp_password"`
	SMTPFrom     string `json:"smtp_from"`
	SMTPSender   string `json:"smtp_sender_name"`
	SMTPTLS      bool   `json:"smtp_tls"`

	// Public app URL used in email templates (verification, reset).
	AppURL string `json:"app_url"`

	// YouTube RTMP defaults for the "Stream to YouTube" recording target.
	// Empty disables YouTube streaming (MP4 still works). Per-recording
	// overrides come via RecordingRequestFrame fields, not here.
	YoutubeRTMPURL   string `json:"youtube_rtmp_url"`
	YoutubeStreamKey string `json:"youtube_stream_key"`

	// ffmpeg audio extraction limits, consumed by ext-rec.
	FFmpegConcurrency int           `json:"ffmpeg_concurrency"` // max simultaneous extractions (default 2)
	FFmpegTimeout     time.Duration `json:"ffmpeg_timeout"`     // per-run deadline (default 10m)

	// readOnly fields mirrored from env. Not editable from the admin UI.
	PublicHost       string `json:"public_host"`        // readOnly
	LivekitPublicURL string `json:"livekit_public_url"` // readOnly
}

// defaultWorldOptions returns the hardcoded defaults used to seed the KV
// bucket on first boot. PUBLIC_HOST and LIVEKIT_PUBLIC_URL are mirrored from
// env (read-only in the admin UI).
func defaultWorldOptions(publicHost, livekitPublicURL string) WorldOptions {
	return WorldOptions{
		SMTPHost:          "mailhog",
		SMTPPort:          1025,
		SMTPFrom:          "noreply@pixeleruv.local",
		SMTPSender:        "PixelEruv",
		AppURL:            fmt.Sprintf("https://%s:4043", publicHost),
		FFmpegConcurrency: 2,
		FFmpegTimeout:     10 * time.Minute,
		PublicHost:        publicHost,
		LivekitPublicURL:  livekitPublicURL,
	}
}

// WorldOptionsManager owns the NATS KV bucket "world_options" and the
// "world_options.update" broadcast. It is the single writer: consumers
// (worldsim itself, ext-rec, frontend) read from KV or subscribe to the
// broadcast.
//
// On startup it creates the bucket if missing, seeds defaults if the key is
// absent, and loads the current value into memory. Set validates, writes to
// KV, updates the in-memory copy, and publishes the update so subscribers
// hot-reload without polling.
type WorldOptionsManager struct {
	nc     *nats.Conn
	js     nats.JetStreamContext
	kv     nats.KeyValue
	logger *slog.Logger

	mu      sync.RWMutex
	current WorldOptions
}

const (
	worldOptionsBucket    = "world_options"
	worldOptionsKey       = "current"
	worldOptionsUpdateSub = "world_options.update"
)

// NewWorldOptionsManager creates the manager, creates/binds the KV bucket,
// seeds defaults if absent, and loads the current value. Returns an error if
// JetStream is unavailable — world_options is required for worldsim to boot.
func NewWorldOptionsManager(nc *nats.Conn, logger *slog.Logger, publicHost, livekitPublicURL string) (*WorldOptionsManager, error) {
	if nc == nil {
		return nil, fmt.Errorf("nats conn is nil")
	}
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream context: %w", err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: worldOptionsBucket,
		TTL:    0, // semi-persistent; survives NATS restart, lost on volume wipe
	})
	if err != nil {
		// Bind in case the bucket already exists and CreateKeyValue fails
		// due to a race (e.g. another worldsim instance created it).
		kv, err = js.KeyValue(worldOptionsBucket)
		if err != nil {
			return nil, fmt.Errorf("create/bind KV bucket %q: %w", worldOptionsBucket, err)
		}
	}

	m := &WorldOptionsManager{nc: nc, js: js, kv: kv, logger: logger}

	// Seed defaults only if the key is absent. Existing deployments keep
	// their stored values across worldsim restarts.
	entry, err := kv.Get(worldOptionsKey)
	if err == nil && entry != nil && len(entry.Value()) > 0 {
		var opts WorldOptions
		if err := json.Unmarshal(entry.Value(), &opts); err != nil {
			return nil, fmt.Errorf("parse stored world_options: %w", err)
		}
		// Backfill readOnly fields from env on every boot — they are not
		// editable from the admin UI and should track the env var.
		opts.PublicHost = publicHost
		opts.LivekitPublicURL = livekitPublicURL
		m.current = opts
		logger.Info("world_options loaded from KV", "smtp_host", opts.SMTPHost, "app_url", opts.AppURL)
		// Re-put so subscribers reading KV see the latest env-mirrored
		// readOnly fields.
		if err := m.putLocked(opts); err != nil {
			logger.Warn("world_options: re-put after env mirror", "err", err)
		}
	} else {
		opts := defaultWorldOptions(publicHost, livekitPublicURL)
		if err := m.putLocked(opts); err != nil {
			return nil, fmt.Errorf("seed world_options: %w", err)
		}
		m.current = opts
		logger.Info("world_options seeded with defaults", "smtp_host", opts.SMTPHost, "app_url", opts.AppURL)
	}

	return m, nil
}

// Get returns a snapshot of the current options. Safe for concurrent use.
func (m *WorldOptionsManager) Get() WorldOptions {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// JSON returns the current options as JSON (for HTTP/NATS replies).
func (m *WorldOptionsManager) JSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(m.current)
}

// Set validates, writes to KV, updates the in-memory copy, and broadcasts
// the new value on "world_options.update". Returns an error if validation
// fails; on success subscribers hot-reload without polling.
//
// readOnly fields (PublicHost, LivekitPublicURL) in `opts` are ignored —
// they are preserved from the current value so env-mirrored values can't be
// overwritten via Set.
func (m *WorldOptionsManager) Set(opts WorldOptions) error {
	if err := validateWorldOptions(opts); err != nil {
		return err
	}
	m.mu.Lock()
	opts.PublicHost = m.current.PublicHost
	opts.LivekitPublicURL = m.current.LivekitPublicURL
	if err := m.putLocked(opts); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("put KV: %w", err)
	}
	m.current = opts
	m.mu.Unlock()

	if err := m.nc.Publish(worldOptionsUpdateSub, mustJSONOpts(opts)); err != nil {
		m.logger.Warn("publish world_options.update", "err", err)
	}
	m.logger.Info("world_options updated", "smtp_host", opts.SMTPHost, "app_url", opts.AppURL,
		"ffmpeg_concurrency", opts.FFmpegConcurrency, "ffmpeg_timeout", opts.FFmpegTimeout)
	return nil
}

// putLocked writes opts to KV. Caller must hold m.mu (or be in a single-
// goroutine init path).
func (m *WorldOptionsManager) putLocked(opts WorldOptions) error {
	data, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	if _, err := m.kv.Put(worldOptionsKey, data); err != nil {
		return err
	}
	return nil
}

// PublishUpdate broadcasts the current options on "world_options.update".
// Called by worldsim on worldsim.ready so late subscribers catch up after a
// worldsim restart without having to read KV.
func (m *WorldOptionsManager) PublishUpdate() {
	data, err := m.JSON()
	if err != nil {
		return
	}
	if err := m.nc.Publish(worldOptionsUpdateSub, data); err != nil {
		m.logger.Warn("publish world_options.update", "err", err)
	}
}

// validateWorldOptions enforces basic sanity. Returns nil if ok.
func validateWorldOptions(opts WorldOptions) error {
	if opts.FFmpegConcurrency < 1 {
		return fmt.Errorf("ffmpeg_concurrency must be >= 1")
	}
	if opts.FFmpegTimeout < 1*time.Second {
		return fmt.Errorf("ffmpeg_timeout must be >= 1s")
	}
	if opts.SMTPHost == "" {
		return fmt.Errorf("smtp_host is required")
	}
	if opts.SMTPPort < 1 || opts.SMTPPort > 65535 {
		return fmt.Errorf("smtp_port must be in 1..65535")
	}
	return nil
}

func mustJSONOpts(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
