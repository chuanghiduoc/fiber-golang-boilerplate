package email

import (
	"context"
	"log/slog"
	"strings"
)

type ConsoleSender struct{}

func NewConsoleSender() *ConsoleSender {
	return &ConsoleSender{}
}

func (s *ConsoleSender) Send(_ context.Context, msg Message) error {
	body := msg.Body
	if body == "" {
		// Reset/verification services currently populate HTML only; dev users
		// still need to see the link.
		body = msg.HTML
	}
	slog.Info("email sent (console driver)",
		slog.String("to", strings.Join(msg.To, ", ")),
		slog.String("subject", msg.Subject),
		slog.String("body", body),
	)
	return nil
}
