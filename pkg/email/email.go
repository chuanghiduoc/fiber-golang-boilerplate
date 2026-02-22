package email

import (
	"context"
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

type Message struct {
	To      []string
	Subject string
	Body    string
	HTML    string
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

func NewSender(cfg config.EmailConfig) (Sender, error) {
	switch cfg.Driver {
	case "smtp":
		return NewSMTPSender(cfg), nil
	case "console":
		return NewConsoleSender(), nil
	default:
		return NewConsoleSender(), nil
	}
}

func formatAddr(name, addr string) string {
	if name == "" {
		return addr
	}
	return fmt.Sprintf("%s <%s>", name, addr)
}
