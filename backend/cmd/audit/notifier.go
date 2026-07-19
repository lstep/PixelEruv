package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats.go"
)

// notifier emails recipients on SeverityError audit events. It fetches
// world_options (SMTP config + recipient mode + king email + custom
// addresses) from worldsim via NATS request-reply at startup and hot-reloads
// on world_options.update. For mode == "all_admins" it calls
// worldsim.admin_emails.get to resolve admin user emails (worldsim owns
// PocketBase; the audit service has no PB access).
//
// Emails are sent in a goroutine so audit persistence is never blocked on
// SMTP. Failures are logged and dropped (best-effort, like audit itself).
type notifier struct {
	nc     *nats.Conn
	logger *slog.Logger

	mu     sync.RWMutex
	opts   notifierOptions
	admins []string // cached admin emails for mode=all_admins
}

type notifierOptions struct {
	SMTPHost                  string
	SMTPPort                  int
	SMTPUsername              string
	SMTPPassword              string
	SMTPFrom                  string
	SMTPSender                string
	SMTPTLS                   bool
	KingEmail                 string
	ErrorEmailRecipientsMode  string
	ErrorEmailCustomAddresses string
}

func newNotifier(nc *nats.Conn, logger *slog.Logger) *notifier {
	n := &notifier{nc: nc, logger: logger}
	// Initial fetch — non-fatal if it fails; we'll retry on the first
	// world_options.update broadcast (worldsim publishes one on ready).
	n.reload()
	// Hot-reload on world_options.update. worldsim broadcasts the full
	// options JSON on every save and on worldsim.ready.
	if _, err := nc.Subscribe("world_options.update", func(m *nats.Msg) {
		n.applyUpdate(m.Data)
	}); err != nil {
		logger.Warn("notifier: subscribe world_options.update", "err", err)
	}
	return n
}

// reload fetches the current world_options from worldsim. Non-fatal on
// error: the notifier stays in its zero-value state (mode="", which means
// "no recipients" — notify returns early).
func (n *notifier) reload() {
	msg, err := n.nc.Request("worldsim.world_options.get", nil, 3*time.Second)
	if err != nil {
		n.logger.Warn("notifier: world_options.get", "err", err)
		return
	}
	n.applyUpdate(msg.Data)
}

// applyUpdate parses a world_options JSON payload and updates the
// notifier's cached options. Called from the initial reload and from the
// world_options.update subscription.
func (n *notifier) applyUpdate(data []byte) {
	var resp struct {
		OK      bool `json:"ok"`
		Error   string `json:"error,omitempty"`
		Options struct {
			SMTPHost                  string `json:"smtp_host"`
			SMTPPort                  int    `json:"smtp_port"`
			SMTPUsername              string `json:"smtp_username"`
			SMTPPassword              string `json:"smtp_password"`
			SMTPFrom                  string `json:"smtp_from"`
			SMTPSender                string `json:"smtp_sender_name"`
			SMTPTLS                   bool   `json:"smtp_tls"`
			KingEmail                 string `json:"king_email"`
			ErrorEmailRecipientsMode  string `json:"error_email_recipients_mode"`
			ErrorEmailCustomAddresses string `json:"error_email_custom_addresses"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		n.logger.Warn("notifier: world_options unmarshal", "err", err)
		return
	}
	if !resp.OK {
		n.logger.Warn("notifier: world_options.get error", "error", resp.Error)
		return
	}
	n.mu.Lock()
	n.opts = notifierOptions{
		SMTPHost:                  resp.Options.SMTPHost,
		SMTPPort:                  resp.Options.SMTPPort,
		SMTPUsername:              resp.Options.SMTPUsername,
		SMTPPassword:              resp.Options.SMTPPassword,
		SMTPFrom:                  resp.Options.SMTPFrom,
		SMTPSender:                resp.Options.SMTPSender,
		SMTPTLS:                   resp.Options.SMTPTLS,
		KingEmail:                 resp.Options.KingEmail,
		ErrorEmailRecipientsMode:  resp.Options.ErrorEmailRecipientsMode,
		ErrorEmailCustomAddresses: resp.Options.ErrorEmailCustomAddresses,
	}
	// Invalidate the admin-emails cache; it will be re-fetched on the next
	// notify() if mode == "all_admins".
	n.admins = nil
	n.mu.Unlock()
	n.logger.Info("notifier: world_options loaded",
		"mode", resp.Options.ErrorEmailRecipientsMode,
		"smtp_host", resp.Options.SMTPHost)
}

// notify sends an error email for the given audit event if the notifier is
// configured (mode != "" and mode != "none"). Called in a goroutine from
// handleAuditEvent so persistence is never blocked on SMTP.
func (n *notifier) notify(ev audit.Event) {
	n.mu.RLock()
	opts := n.opts
	n.mu.RUnlock()
	if opts.ErrorEmailRecipientsMode == "" || opts.ErrorEmailRecipientsMode == "none" {
		return
	}
	if opts.SMTPHost == "" {
		n.logger.Warn("notifier: smtp_host empty, skipping error email", "event", ev.EventType)
		return
	}
	recipients := n.resolveRecipients(opts)
	if len(recipients) == 0 {
		n.logger.Warn("notifier: no recipients resolved, skipping error email", "event", ev.EventType, "mode", opts.ErrorEmailRecipientsMode)
		return
	}
	if err := n.sendEmail(opts, recipients, ev); err != nil {
		n.logger.Warn("notifier: send error email", "err", err, "event", ev.EventType, "recipients", len(recipients))
	} else {
		n.logger.Info("notifier: error email sent", "event", ev.EventType, "recipients", len(recipients))
	}
}

// resolveRecipients builds the To list based on the configured mode. For
// mode == "all_admins" it fetches admin emails via worldsim.admin_emails.get
// (cached for 5 minutes to avoid hammering PB on every error).
func (n *notifier) resolveRecipients(opts notifierOptions) []string {
	switch opts.ErrorEmailRecipientsMode {
	case "king":
		if opts.KingEmail == "" {
			return nil
		}
		return []string{opts.KingEmail}
	case "custom":
		var out []string
		for _, a := range strings.Split(opts.ErrorEmailCustomAddresses, ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				out = append(out, a)
			}
		}
		return out
	case "all_admins":
		n.mu.Lock()
		cached := n.admins
		n.mu.Unlock()
		if len(cached) > 0 {
			return cached
		}
		msg, err := n.nc.Request("worldsim.admin_emails.get", nil, 3*time.Second)
		if err != nil {
			n.logger.Warn("notifier: admin_emails.get", "err", err)
			return nil
		}
		var resp struct {
			OK     bool     `json:"ok"`
			Error  string   `json:"error,omitempty"`
			Emails []string `json:"emails"`
		}
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			n.logger.Warn("notifier: admin_emails unmarshal", "err", err)
			return nil
		}
		if !resp.OK {
			n.logger.Warn("notifier: admin_emails.get error", "error", resp.Error)
			return nil
		}
		n.mu.Lock()
		n.admins = resp.Emails
		n.mu.Unlock()
		return resp.Emails
	}
	return nil
}

// sendEmail sends a plain-text error notification via SMTP. Uses net/smtp
// directly (the audit service doesn't depend on PocketBase's mailer).
func (n *notifier) sendEmail(opts notifierOptions, to []string, ev audit.Event) error {
	from := opts.SMTPFrom
	if from == "" {
		from = "noreply@pixeleruv.local"
	}
	sender := opts.SMTPSender
	if sender == "" {
		sender = "PixelEruv Audit"
	}
	subject := fmt.Sprintf("[PixelEruv] Error: %s", ev.EventType)
	body := fmt.Sprintf("An error event was recorded by the PixelEruv audit system.\n\n"+
		"Event type: %s\nSeverity: %s\nTime: %s\n\n"+
		"Actor:\n  entity_id: %s\n  client_id: %s\n  sub: %s\n  ip: %s\n  extension: %s\n\n"+
		"Details: %s\n\n"+
		"View the full event at the audit dashboard.\n",
		ev.EventType, ev.Severity, ev.Timestamp,
		ev.Actor.EntityID, ev.Actor.ClientID, ev.Actor.Sub, ev.Actor.IP, ev.Actor.Extension,
		detailsString(ev.Details))

	msg := buildRFC822(from, sender, to, subject, body)
	addr := fmt.Sprintf("%s:%d", opts.SMTPHost, opts.SMTPPort)
	var auth smtp.Auth
	if opts.SMTPUsername != "" {
		auth = smtp.PlainAuth("", opts.SMTPUsername, opts.SMTPPassword, opts.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, to, []byte(msg))
}

// buildRFC822 assembles a minimal RFC 822 message with a From header, To
// header, Subject, and plain-text body.
func buildRFC822(from, sender string, to []string, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s <%s>\r\n", sender, from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// detailsString renders the audit Details map as a compact JSON string for
// the email body. Falls back to "<none>" if empty.
func detailsString(d audit.Details) string {
	if len(d) == 0 {
		return "<none>"
	}
	data, err := json.Marshal(d)
	if err != nil {
		return "<unmarshal error>"
	}
	return string(data)
}
