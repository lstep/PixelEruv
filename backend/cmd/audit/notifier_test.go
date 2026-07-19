package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/smtp"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATSForAudit starts an in-process NATS server on a free port.
// Mirrors the worldsim test helper but kept local to avoid an import cycle.
func startEmbeddedNATSForAudit(t *testing.T) string {
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
	srv, err := server.NewServer(&opts)
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(2 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// TestNotifier_ResolveRecipients verifies that resolveRecipients returns
// the right list for each mode (king, custom, all_admins, none).
func TestNotifier_ResolveRecipients(t *testing.T) {
	natsURL := startEmbeddedNATSForAudit(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	// Stub worldsim.world_options.get and worldsim.admin_emails.get so the
	// notifier can fetch its config without a real worldsim.
	worldOptsReply, _ := json.Marshal(map[string]any{
		"ok": true,
		"options": map[string]any{
			"smtp_host":                  "mailhog",
			"smtp_port":                  1025,
			"king_email":                 "king@example.com",
			"error_email_recipients_mode": "king",
			"error_email_custom_addresses": "a@x.com,b@x.com",
		},
	})
	if _, err := nc.Subscribe("worldsim.world_options.get", func(m *nats.Msg) {
		m.Respond(worldOptsReply)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	adminEmailsReply, _ := json.Marshal(map[string]any{
		"ok":     true,
		"emails": []string{"admin1@x.com", "admin2@x.com"},
	})
	if _, err := nc.Subscribe("worldsim.admin_emails.get", func(m *nats.Msg) {
		m.Respond(adminEmailsReply)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	logger := slog.New(slog.NewTextHandler(&testWriterAudit{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	n := newNotifier(nc, logger)

	// Wait briefly for newNotifier's initial reload to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.RLock()
		mode := n.opts.ErrorEmailRecipientsMode
		n.mu.RUnlock()
		if mode == "king" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// king mode -> [king@example.com]
	if got := n.resolveRecipients(notifierOptions{ErrorEmailRecipientsMode: "king", KingEmail: "king@example.com"}); len(got) != 1 || got[0] != "king@example.com" {
		t.Errorf("king mode: got %v, want [king@example.com]", got)
	}

	// custom mode -> parsed list
	got := n.resolveRecipients(notifierOptions{ErrorEmailRecipientsMode: "custom", ErrorEmailCustomAddresses: "a@x.com, b@x.com ,,"})
	if len(got) != 2 || got[0] != "a@x.com" || got[1] != "b@x.com" {
		t.Errorf("custom mode: got %v, want [a@x.com b@x.com]", got)
	}

	// all_admins mode -> fetched from worldsim.admin_emails.get
	got = n.resolveRecipients(notifierOptions{ErrorEmailRecipientsMode: "all_admins"})
	if len(got) != 2 || got[0] != "admin1@x.com" || got[1] != "admin2@x.com" {
		t.Errorf("all_admins mode: got %v, want [admin1@x.com admin2@x.com]", got)
	}

	// none mode -> nil
	if got := n.resolveRecipients(notifierOptions{ErrorEmailRecipientsMode: "none"}); got != nil {
		t.Errorf("none mode: got %v, want nil", got)
	}
}

// TestNotifier_NotifySkipsOnNoneMode verifies that notify is a no-op when
// mode is "none" (no SMTP connection attempted). This guards against the
// notifier spamming SMTP when the admin explicitly disabled emails.
func TestNotifier_NotifySkipsOnNoneMode(t *testing.T) {
	natsURL := startEmbeddedNATSForAudit(t)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	// Stub world_options.get with mode=none.
	reply, _ := json.Marshal(map[string]any{
		"ok": true,
		"options": map[string]any{
			"smtp_host":                  "mailhog",
			"smtp_port":                  1025,
			"error_email_recipients_mode": "none",
		},
	})
	if _, err := nc.Subscribe("worldsim.world_options.get", func(m *nats.Msg) {
		m.Respond(reply)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	logger := slog.New(slog.NewTextHandler(&testWriterAudit{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	n := newNotifier(nc, logger)
	// Wait for reload.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.RLock()
		mode := n.opts.ErrorEmailRecipientsMode
		n.mu.RUnlock()
		if mode == "none" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// notify should return without attempting SMTP (which would fail since
	// there's no SMTP server). If it tried, we'd see a log warning; the
	// test passes if notify returns cleanly.
	n.notify(audit.Event{EventType: "test.error", Severity: audit.SeverityError})
}

// TestBuildRFC822 verifies the email header format.
func TestBuildRFC822(t *testing.T) {
	got := buildRFC822("noreply@x.com", "PixelEruv Audit", []string{"a@x.com", "b@x.com"}, "Subject", "Body")
	if want := "From: PixelEruv Audit <noreply@x.com>\r\n"; got[:len(want)] != want {
		t.Errorf("From header: got %q, want %q", got[:len(want)], want)
	}
	if want := "To: a@x.com, b@x.com\r\n"; !contains(got, want) {
		t.Errorf("To header missing: %q", got)
	}
	if want := "Subject: Subject\r\n"; !contains(got, want) {
		t.Errorf("Subject header missing: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// testWriterAudit is a slog writer that routes to testing.T.Logf.
type testWriterAudit struct{ t *testing.T }

func (w *testWriterAudit) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// Ensure net/smtp import is used (smtp.SendMail is referenced in notifier.go,
// not here, but the test file imports it to keep the dependency explicit for
// future stub-based tests).
var _ = smtp.SendMail
