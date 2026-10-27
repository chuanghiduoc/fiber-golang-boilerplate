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

// smtpTimeout bounds the TCP connect and, as a connection deadline, the whole
// SMTP conversation.
const smtpTimeout = 30 * time.Second

// Send delivers msg in one SMTP conversation: connect, STARTTLS, AUTH,
// MAIL/RCPT, DATA, QUIT.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	message := s.buildMessage(msg)

	conn, stopWatch, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer stopWatch()

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer client.Close()

	if err := s.startTLS(client); err != nil {
		return err
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	if err := s.transmit(client, msg.To, message); err != nil {
		return err
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

// buildMessage renders the RFC 5322 headers and body. Header values are
// sanitized against CRLF injection.
func (s *SMTPSender) buildMessage(msg Message) string {
	headers := map[string]string{
		"From":         sanitizeHeader(formatAddr(s.fromName, s.from)),
		"To":           sanitizeHeader(strings.Join(msg.To, ", ")),
		"Subject":      sanitizeHeader(msg.Subject),
		"MIME-Version": "1.0",
	}

	body := msg.Body
	headers["Content-Type"] = "text/plain; charset=UTF-8"
	if msg.HTML != "" {
		headers["Content-Type"] = "text/html; charset=UTF-8"
		body = msg.HTML
	}

	var message strings.Builder
	for k, v := range headers {
		fmt.Fprintf(&message, "%s: %s\r\n", k, v)
	}
	message.WriteString("\r\n")
	message.WriteString(body)
	return message.String()
}

// dial connects to the server with a context-aware dialer. smtp.Client has no
// context-aware methods, so a connection deadline plus a watcher that expires
// it on cancellation keeps a stalled server from leaking the connection or
// goroutine. The returned stop function ends the watcher.
func (s *SMTPSender) dial(ctx context.Context) (conn net.Conn, stop func(), err error) {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	dialer := net.Dialer{Timeout: smtpTimeout}
	conn, err = dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to SMTP server: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("set SMTP deadline: %w", err)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()
	return conn, func() { close(done) }, nil
}

// startTLS upgrades to TLS via STARTTLS when the server advertises it, so
// credentials and message content (reset/verification links) are not sent in
// cleartext. Without STARTTLS the send fails only when TLS is required.
func (s *SMTPSender) startTLS(client *smtp.Client) error {
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("SMTP STARTTLS: %w", err)
		}
		return nil
	}
	if s.requireTLS {
		return fmt.Errorf("SMTP server %q does not support STARTTLS but SMTP_REQUIRE_TLS is enabled", s.host)
	}
	return nil
}

// transmit runs the MAIL FROM, RCPT TO, and DATA exchange.
func (s *SMTPSender) transmit(client *smtp.Client, recipients []string, message string) error {
	if err := client.Mail(s.from); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	for _, to := range recipients {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("SMTP RCPT TO: %w", err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write([]byte(message)); err != nil {
		_ = w.Close()
		return fmt.Errorf("write SMTP message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close SMTP data: %w", err)
	}
	return nil
}
