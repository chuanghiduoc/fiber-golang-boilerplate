package email

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type SMTPSender struct {
	host     string
	port     int
	username string
	password string
	from     string
	fromName string
}

func NewSMTPSender(cfg config.EmailConfig) *SMTPSender {
	return &SMTPSender{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		username: cfg.SMTPUsername,
		password: cfg.SMTPPassword,
		from:     cfg.FromAddress,
		fromName: cfg.FromName,
	}
}

func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	from := formatAddr(s.fromName, s.from)

	headers := map[string]string{
		"From":         from,
		"To":           strings.Join(msg.To, ", "),
		"Subject":      msg.Subject,
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

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("create SMTP client: %w", err)
	}
	defer client.Close()

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

	return client.Quit()
}
