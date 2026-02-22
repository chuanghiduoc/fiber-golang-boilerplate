package email

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"

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

func (s *SMTPSender) Send(_ context.Context, msg Message) error {
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

	var auth smtp.Auth
	if s.username != "" {
		auth = smtp.PlainAuth("", s.username, s.password, s.host)
	}

	return smtp.SendMail(addr, auth, s.from, msg.To, []byte(message.String()))
}
