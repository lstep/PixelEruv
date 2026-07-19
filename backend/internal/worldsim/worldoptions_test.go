package worldsim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATSWithJetStream starts an in-process NATS server with
// JetStream enabled, for tests that need KV buckets. The default
// startEmbeddedNATS helper does not enable JetStream.
func startEmbeddedNATSWithJetStream(t *testing.T) (*server.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	host, port, _ := net.SplitHostPort(addr)
	opts := test.DefaultTestOptions
	opts.Host = host
	opts.Port, _ = net.LookupPort("tcp", port)
	opts.JetStream = true
	opts.StoreDir = t.TempDir()
	srv, err := server.NewServer(&opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv, fmt.Sprintf("nats://%s", addr)
}

// TestWorldOptionsManager_SeedDefaults verifies that a fresh KV bucket is
// seeded with hardcoded defaults on first boot, and that Get returns the
// seeded values.
func TestWorldOptionsManager_SeedDefaults(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := NewWorldOptionsManager(nc, logger, "testhost.example", "ws://testhost:7880")
	if err != nil {
		t.Fatalf("NewWorldOptionsManager: %v", err)
	}

	opts := mgr.Get()
	if opts.SMTPHost != "mailhog" {
		t.Errorf("SMTPHost = %q, want mailhog", opts.SMTPHost)
	}
	if opts.SMTPPort != 1025 {
		t.Errorf("SMTPPort = %d, want 1025", opts.SMTPPort)
	}
	if opts.FFmpegConcurrency != 2 {
		t.Errorf("FFmpegConcurrency = %d, want 2", opts.FFmpegConcurrency)
	}
	if opts.FFmpegTimeout != 10*time.Minute {
		t.Errorf("FFmpegTimeout = %v, want 10m", opts.FFmpegTimeout)
	}
	if opts.PublicHost != "testhost.example" {
		t.Errorf("PublicHost = %q, want testhost.example", opts.PublicHost)
	}
	if opts.LivekitPublicURL != "ws://testhost:7880" {
		t.Errorf("LivekitPublicURL = %q, want ws://testhost:7880", opts.LivekitPublicURL)
	}
}

// TestWorldOptionsManager_PersistAcrossInstances verifies that a second
// manager instance binding the same KV bucket loads the previously-stored
// values rather than re-seeding defaults.
func TestWorldOptionsManager_PersistAcrossInstances(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr1, err := NewWorldOptionsManager(nc, logger, "host1", "ws://host1:7880")
	if err != nil {
		t.Fatalf("mgr1: %v", err)
	}
	// Mutate and persist.
	if err := mgr1.Set(WorldOptions{
		SMTPHost:                 "smtp.example.com",
		SMTPPort:                 587,
		FFmpegConcurrency:        4,
		FFmpegTimeout:            5 * time.Minute,
		AppURL:                   "https://example.com",
		ErrorEmailRecipientsMode: "none",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Second instance on the same NATS — should load the stored values.
	mgr2, err := NewWorldOptionsManager(nc, logger, "host2", "ws://host2:7880")
	if err != nil {
		t.Fatalf("mgr2: %v", err)
	}
	opts := mgr2.Get()
	if opts.SMTPHost != "smtp.example.com" {
		t.Errorf("SMTPHost = %q, want smtp.example.com", opts.SMTPHost)
	}
	if opts.FFmpegConcurrency != 4 {
		t.Errorf("FFmpegConcurrency = %d, want 4", opts.FFmpegConcurrency)
	}
	// readOnly fields are re-mirrored from env on every boot, so host2 wins.
	if opts.PublicHost != "host2" {
		t.Errorf("PublicHost = %q, want host2 (re-mirrored from env)", opts.PublicHost)
	}
}

// TestWorldOptionsManager_SetBroadcastsUpdate verifies that Set publishes
// the new options on the world_options.update NATS subject so subscribers
// hot-reload without polling.
func TestWorldOptionsManager_SetBroadcastsUpdate(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := NewWorldOptionsManager(nc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("mgr: %v", err)
	}

	got := make(chan WorldOptions, 1)
	sub, err := nc.Subscribe(worldOptionsUpdateSub, func(m *nats.Msg) {
		var opts WorldOptions
		if err := json.Unmarshal(m.Data, &opts); err != nil {
			t.Errorf("unmarshal: %v", err)
			return
		}
		select {
		case got <- opts:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })
	nc.Flush()

	if err := mgr.Set(WorldOptions{
		SMTPHost:                 "broadcast.example.com",
		SMTPPort:                 2525,
		FFmpegConcurrency:        3,
		FFmpegTimeout:            2 * time.Minute,
		ErrorEmailRecipientsMode: "none",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	select {
	case opts := <-got:
		if opts.SMTPHost != "broadcast.example.com" {
			t.Errorf("broadcast SMTPHost = %q, want broadcast.example.com", opts.SMTPHost)
		}
		if opts.FFmpegConcurrency != 3 {
			t.Errorf("broadcast FFmpegConcurrency = %d, want 3", opts.FFmpegConcurrency)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive world_options.update broadcast")
	}
}

// TestWorldOptionsManager_SetRejectsInvalid verifies that validation
// prevents storing nonsensical values.
func TestWorldOptionsManager_SetRejectsInvalid(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := NewWorldOptionsManager(nc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("mgr: %v", err)
	}

	cases := []struct {
		name string
		opts WorldOptions
	}{
		{"zero concurrency", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 0, FFmpegTimeout: time.Minute}},
		{"zero timeout", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: 0}},
		{"empty host", WorldOptions{SMTPHost: "", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute}},
		{"bad port", WorldOptions{SMTPHost: "h", SMTPPort: 99999, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute}},
		{"king mode without king_email", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute, ErrorEmailRecipientsMode: "king"}},
		{"custom mode without addresses", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute, ErrorEmailRecipientsMode: "custom"}},
		{"custom mode with bad email", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute, ErrorEmailRecipientsMode: "custom", ErrorEmailCustomAddresses: "not-an-email"}},
		{"bad king_email", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute, KingEmail: "no-at-sign"}},
		{"bad recipients mode", WorldOptions{SMTPHost: "h", SMTPPort: 25, FFmpegConcurrency: 1, FFmpegTimeout: time.Minute, ErrorEmailRecipientsMode: "bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := mgr.Set(c.opts); err == nil {
				t.Error("Set succeeded, want validation error")
			}
		})
	}
}

// TestWorldOptionsManager_NewFieldsPersist verifies that the king / error-
// email / recording fields round-trip through Set + Get (KV persistence).
func TestWorldOptionsManager_NewFieldsPersist(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := NewWorldOptionsManager(nc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("mgr: %v", err)
	}
	want := WorldOptions{
		SMTPHost:                  "smtp.example.com",
		SMTPPort:                  587,
		FFmpegConcurrency:         2,
		FFmpegTimeout:             5 * time.Minute,
		KingName:                  "Lord Pixel",
		KingEmail:                 "king@example.com",
		ErrorEmailRecipientsMode:  "custom",
		ErrorEmailCustomAddresses: "a@x.com, b@x.com",
		RecordingEnabled:          false,
	}
	if err := mgr.Set(want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Re-bind the same bucket to simulate a worldsim restart.
	mgr2, err := NewWorldOptionsManager(nc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("mgr2: %v", err)
	}
	got := mgr2.Get()
	if got.KingName != want.KingName {
		t.Errorf("KingName = %q, want %q", got.KingName, want.KingName)
	}
	if got.KingEmail != want.KingEmail {
		t.Errorf("KingEmail = %q, want %q", got.KingEmail, want.KingEmail)
	}
	if got.ErrorEmailRecipientsMode != want.ErrorEmailRecipientsMode {
		t.Errorf("ErrorEmailRecipientsMode = %q, want %q", got.ErrorEmailRecipientsMode, want.ErrorEmailRecipientsMode)
	}
	if got.ErrorEmailCustomAddresses != want.ErrorEmailCustomAddresses {
		t.Errorf("ErrorEmailCustomAddresses = %q, want %q", got.ErrorEmailCustomAddresses, want.ErrorEmailCustomAddresses)
	}
	if got.RecordingEnabled != want.RecordingEnabled {
		t.Errorf("RecordingEnabled = %v, want %v", got.RecordingEnabled, want.RecordingEnabled)
	}
}

// TestWorldOptionsManager_RecordingEnabledDefault verifies that a fresh
// bucket seeds RecordingEnabled=true and ErrorEmailRecipientsMode="king".
func TestWorldOptionsManager_RecordingEnabledDefault(t *testing.T) {
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mgr, err := NewWorldOptionsManager(nc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("mgr: %v", err)
	}
	opts := mgr.Get()
	if !opts.RecordingEnabled {
		t.Error("RecordingEnabled default = false, want true")
	}
	if opts.ErrorEmailRecipientsMode != "king" {
		t.Errorf("ErrorEmailRecipientsMode default = %q, want king", opts.ErrorEmailRecipientsMode)
	}
}
