package email

import (
	"context"
	"fmt"
	"net/mail"

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
		return nil, fmt.Errorf("unknown email driver: %q (must be one of: console, smtp)", cfg.Driver)
	}
}

func formatAddr(name, addr string) string {
	name = sanitizeHeader(name)
	addr = sanitizeHeader(addr)
	if name == "" {
		return addr
	}
	// mail.Address quotes/encodes the display name. Building "%s <%s>"
	// manually lets a name containing a comma or quote corrupt the header.
	return (&mail.Address{Name: name, Address: addr}).String()
}
