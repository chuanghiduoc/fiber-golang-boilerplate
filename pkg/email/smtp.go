package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type SMTPSender struct {
	host       string
	port       int
	username   string
	password   string
	from       string
	fromName   string
	requireTLS bool
}

func NewSMTPSender(cfg config.EmailConfig) *SMTPSender {
	return &SMTPSender{
		host:       cfg.SMTPHost,
		port:       cfg.SMTPPort,
		username:   cfg.SMTPUsername,
		password:   cfg.SMTPPassword,
		from:       cfg.FromAddress,
		fromName:   cfg.FromName,
		requireTLS: cfg.SMTPRequireTLS,
	}
}

// sanitizeHeader strips CR and LF characters to prevent SMTP header injection.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	from := formatAddr(s.fromName, s.from)

	headers := map[string]string{
		"From":         sanitizeHeader(from),
		"To":           sanitizeHeader(strings.Join(msg.To, ", ")),
		"Subject":      sanitizeHeader(msg.Subject),
		"MIME-Version": "1.0",
	}

	var body string
	if msg.HTML != "" {
		headers["Content-Type"] = "text/html; charset=UTF-8"
		body = msg.HTML
	} else {
		headers["Content-Type"] = "text/plain; charset=UTF-8"
		body = msg.Body
	}

	var message strings.Builder
	for k, v := range headers {
		fmt.Fprintf(&message, "%s: %s\r\n", k, v)
	}
	message.WriteString("\r\n")
	message.WriteString(body)

	// Use context-aware dialer with 30s default timeout
	dialer := net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect to SMTP server: %w", err)
	}
	// smtp.Client has no context-aware methods. A deadline plus a watcher keeps
	// a stalled server from leaking the connection/goroutine forever.
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("set SMTP deadline: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer client.Close()

	// Upgrade to TLS via STARTTLS when the server advertises it, so credentials
	// and message content (reset/verification links) are not sent in cleartext.
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS: %w", err)
		}
	} else if s.requireTLS {
		return fmt.Errorf("SMTP server %q does not support STARTTLS but SMTP_REQUIRE_TLS is enabled", s.host)
	}

	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	for _, to := range msg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("SMTP RCPT TO: %w", err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}

	if _, err := w.Write([]byte(message.String())); err != nil {
		w.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("close SMTP data: %w", err)
	}

	// The server accepted the message once DATA closed successfully. A QUIT
	// failure must not fail the send, or the email job would retry and deliver
	// a duplicate.
	if err := client.Quit(); err != nil {
		slog.Warn("SMTP QUIT failed after the message was accepted",
			slog.String("host", s.host),
			slog.String("error", err.Error()),
		)
	}
	return nil
}
