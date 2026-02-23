package email

import (
	"context"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func TestNewConsoleSender(t *testing.T) {
	s := NewConsoleSender()
	if s == nil {
		t.Fatal("NewConsoleSender() returned nil")
	}
}

func TestConsoleSender_Send(t *testing.T) {
	s := NewConsoleSender()
	err := s.Send(context.Background(), Message{
		To:      []string{"test@example.com"},
		Subject: "Test Subject",
		Body:    "Test Body",
	})
	if err != nil {
		t.Errorf("Send() returned error: %v", err)
	}
}

func TestNewSender_Console(t *testing.T) {
	sender, err := NewSender(config.EmailConfig{Driver: "console"})
	if err != nil {
		t.Fatalf("NewSender(console) returned error: %v", err)
	}
	if sender == nil {
		t.Fatal("NewSender(console) returned nil")
	}
}

func TestNewSender_Default(t *testing.T) {
	sender, err := NewSender(config.EmailConfig{Driver: "unknown"})
	if err != nil {
		t.Fatalf("NewSender(unknown) returned error: %v", err)
	}
	if sender == nil {
		t.Fatal("NewSender(unknown) returned nil")
	}
}

func TestNewSender_SMTP(t *testing.T) {
	sender, err := NewSender(config.EmailConfig{
		Driver:   "smtp",
		SMTPHost: "smtp.example.com",
		SMTPPort: 587,
	})
	if err != nil {
		t.Fatalf("NewSender(smtp) returned error: %v", err)
	}
	if sender == nil {
		t.Fatal("NewSender(smtp) returned nil")
	}
}

func TestNewSMTPSender(t *testing.T) {
	s := NewSMTPSender(config.EmailConfig{
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "user",
		SMTPPassword: "pass",
		FromAddress:  "noreply@example.com",
		FromName:     "Test App",
	})
	if s == nil {
		t.Fatal("NewSMTPSender returned nil")
	}
	if s.host != "smtp.example.com" {
		t.Errorf("host = %q, want smtp.example.com", s.host)
	}
	if s.port != 587 {
		t.Errorf("port = %d, want 587", s.port)
	}
}

func TestFormatAddr(t *testing.T) {
	tests := []struct {
		name, addr, want string
	}{
		{"Test User", "test@example.com", "Test User <test@example.com>"},
		{"", "test@example.com", "test@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name+"_"+tt.addr, func(t *testing.T) {
			got := formatAddr(tt.name, tt.addr)
			if got != tt.want {
				t.Errorf("formatAddr(%q, %q) = %q, want %q", tt.name, tt.addr, got, tt.want)
			}
		})
	}
}
