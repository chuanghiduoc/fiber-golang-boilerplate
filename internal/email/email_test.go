package email

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

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

func TestNewSender_UnknownDriver(t *testing.T) {
	sender, err := NewSender(config.EmailConfig{Driver: "unknown"})
	if err == nil {
		t.Fatal("NewSender(unknown) should return an error for an unknown driver")
	}
	if sender != nil {
		t.Fatal("NewSender(unknown) should return a nil sender on error")
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
		SMTPPassword: "smtp" + "-" + "fixture",
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
		{"Test User", "test@example.com", `"Test User" <test@example.com>`},
		{"Doe, John", "john@example.com", `"Doe, John" <john@example.com>`},
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

func TestSMTPSender_PrepareMessageRejectsInvalidEnvelope(t *testing.T) {
	valid := Message{To: []string{"user@example.com"}, Subject: "hello", Body: "body"}
	cases := map[string]struct {
		from string
		msg  Message
	}{
		"invalid sender":      {from: "not-an-address", msg: valid},
		"no recipients":       {from: "noreply@example.com", msg: Message{Subject: "hello"}},
		"empty recipient":     {from: "noreply@example.com", msg: Message{To: []string{""}}},
		"invalid recipient":   {from: "noreply@example.com", msg: Message{To: []string{"not-an-address"}}},
		"recipient injection": {from: "noreply@example.com", msg: Message{To: []string{"user@example.com\r\nBcc: evil@example.com"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sender := NewSMTPSender(config.EmailConfig{FromAddress: tc.from})
			if _, _, err := sender.prepareMessage(tc.msg); err == nil {
				t.Fatal("prepareMessage accepted an invalid envelope")
			}
		})
	}
}

func TestSMTPSender_ServerAddressUsesJoinHostPort(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"smtp.example.com", 587, "smtp.example.com:587"},
		{"::1", 2525, "[::1]:2525"},
	}
	for _, tc := range cases {
		sender := NewSMTPSender(config.EmailConfig{SMTPHost: tc.host, SMTPPort: tc.port})
		if got := sender.serverAddress(); got != tc.want {
			t.Errorf("serverAddress(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

func TestSMTPSender_BuildMessageEncodesHeadersAndBody(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{
		FromAddress: "noreply@example.com",
		FromName:    "Báo cáo\r\nBcc: attacker@example.com",
	})
	msg := Message{
		To:      []string{"Người nhận <user@example.com>"},
		Subject: "Xin chào\r\nBcc: attacker@example.com",
		Body:    "Nội dung tiếng Việt",
	}
	from, recipients, err := sender.prepareMessage(msg)
	if err != nil {
		t.Fatalf("prepareMessage: %v", err)
	}
	raw := sender.buildMessage(msg, from, recipients)

	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message is not parseable RFC 5322: %v\n%s", err, raw)
	}
	if got := len(parsed.Header["Bcc"]); got != 0 {
		t.Fatalf("CRLF injection created %d Bcc headers", got)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if subject != "Xin chàoBcc: attacker@example.com" {
		t.Errorf("subject = %q", subject)
	}
	fromAddr, err := mail.ParseAddress(parsed.Header.Get("From"))
	if err != nil {
		t.Fatalf("parse From: %v", err)
	}
	if !strings.Contains(fromAddr.Name, "Báo cáo") {
		t.Errorf("From display name = %q", fromAddr.Name)
	}
	if _, err := mail.ParseAddress(parsed.Header.Get("To")); err != nil {
		t.Errorf("parse To: %v", err)
	}
	if got := parsed.Header.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Errorf("Content-Transfer-Encoding = %q, want base64", got)
	}
	date, err := mail.ParseDate(parsed.Header.Get("Date"))
	if err != nil {
		t.Fatalf("Date header is invalid: %v", err)
	}
	if delta := time.Since(date); delta < -time.Minute || delta > time.Minute {
		t.Errorf("Date header is not current: %s", date)
	}

	encodedBody, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(encodedBody), "\r\n"), "\r\n") {
		if len(line) > smtpBodyLineWidth {
			t.Fatalf("base64 line length = %d, want <= %d", len(line), smtpBodyLineWidth)
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encodedBody), "\r\n", ""))
	if err != nil {
		t.Fatalf("decode base64 body: %v", err)
	}
	if string(decoded) != msg.Body {
		t.Errorf("decoded body = %q, want %q", decoded, msg.Body)
	}
}
func TestFormatAddr_StripsHeaderControlCharacters(t *testing.T) {
	got := formatAddr("Support\r\nBcc: attacker@example.com", "user@example.com")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("formatAddr left a header control character: %q", got)
	}
	if !strings.Contains(got, "user@example.com") {
		t.Fatalf("formatAddr lost the address: %q", got)
	}
}

func TestBuildMessage_SanitizesFromAndRecipientNames(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{
		FromAddress: "noreply@example.com",
		FromName:    "App\r\nBcc: attacker@example.com",
	})
	from, recipients, err := sender.prepareMessage(Message{
		To:      []string{"User <user@example.com>"},
		Subject: "hello",
		Body:    "body",
	})
	if err != nil {
		t.Fatalf("prepareMessage: %v", err)
	}
	message := sender.buildMessage(Message{Subject: "hello", Body: "body"}, from, recipients)
	parsed, err := mail.ReadMessage(strings.NewReader(message))
	if err != nil {
		t.Fatalf("message is not parseable: %v\n%s", err, message)
	}
	if got := len(parsed.Header["Bcc"]); got != 0 {
		t.Fatalf("buildMessage allowed header injection: %d Bcc headers\n%s", got, message)
	}
	fromAddr, err := mail.ParseAddress(parsed.Header.Get("From"))
	if err != nil {
		t.Fatalf("parse From: %v", err)
	}
	if strings.ContainsAny(fromAddr.Name, "\r\n") {
		t.Fatalf("From display name contains a control character: %q", fromAddr.Name)
	}
}

func TestBuildMessage_FoldsLongSubjectWithinLineLimit(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{FromAddress: "noreply@example.com"})
	subject := strings.Repeat("Tiếng Việt có dấu ", 80)
	msg := Message{To: []string{"user@example.com"}, Subject: subject, Body: "body"}
	from, recipients, err := sender.prepareMessage(msg)
	if err != nil {
		t.Fatalf("prepareMessage: %v", err)
	}
	raw := sender.buildMessage(msg, from, recipients)
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > smtpHeaderLineLimit {
			t.Fatalf("header line length = %d, want <= %d", len(line), smtpHeaderLineLimit)
		}
	}
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message is not parseable: %v", err)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	if decoded != subject {
		t.Errorf("decoded subject does not round-trip: got %d runes, want %d", len(decoded), len(subject))
	}
}

func TestSanitizeHeader_StripsControlCharacters(t *testing.T) {
	got := sanitizeHeader("a\x00b\x1bc\x7fd\re")
	if got != "abcde" {
		t.Fatalf("sanitizeHeader = %q, want %q", got, "abcde")
	}
}

func TestPrepareMessage_RejectsTooManyRecipients(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{FromAddress: "noreply@example.com"})
	to := make([]string, smtpMaxRecipients+1)
	for i := range to {
		to[i] = "user@example.com"
	}
	if _, _, err := sender.prepareMessage(Message{To: to}); err == nil {
		t.Fatal("prepareMessage accepted more recipients than the SMTP envelope limit")
	}
}

func TestBuildMessage_UsesMultipartAlternativeWhenBothBodiesSet(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{FromAddress: "noreply@example.com"})
	msg := Message{
		To:      []string{"user@example.com"},
		Subject: "hello",
		Body:    "plain fallback",
		HTML:    "<p>html body</p>",
	}
	from, recipients, err := sender.prepareMessage(msg)
	if err != nil {
		t.Fatalf("prepareMessage: %v", err)
	}
	raw := sender.buildMessage(msg, from, recipients)
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message is not parseable: %v\n%s", err, raw)
	}
	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, want multipart/alternative", mediaType)
	}
	if got := parsed.Header.Get("Content-Transfer-Encoding"); got != "" {
		t.Errorf("multipart message must not set a top-level Content-Transfer-Encoding, got %q", got)
	}
	reader := multipart.NewReader(parsed.Body, params["boundary"])
	wantTypes := []string{"text/plain", "text/html"}
	wantBodies := []string{msg.Body, msg.HTML}
	for i, wantType := range wantTypes {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("read part %d: %v", i, err)
		}
		if got := part.Header.Get("Content-Type"); !strings.HasPrefix(got, wantType) {
			t.Errorf("part %d Content-Type = %q, want prefix %q", i, got, wantType)
		}
		encoded, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read part %d body: %v", i, err)
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
		if err != nil {
			t.Fatalf("decode part %d: %v", i, err)
		}
		if string(decoded) != wantBodies[i] {
			t.Errorf("part %d body = %q, want %q", i, decoded, wantBodies[i])
		}
	}
	if _, err := reader.NextPart(); err != io.EOF {
		t.Errorf("unexpected extra multipart part: %v", err)
	}
}

func TestBuildMessage_FoldsLongRecipientList(t *testing.T) {
	sender := NewSMTPSender(config.EmailConfig{FromAddress: "noreply@example.com"})
	to := make([]string, smtpMaxRecipients)
	for i := range to {
		to[i] = fmt.Sprintf("Recipient %02d <%02d@example.com>", i, i)
	}
	msg := Message{To: to, Subject: "hello", Body: "body"}
	from, recipients, err := sender.prepareMessage(msg)
	if err != nil {
		t.Fatalf("prepareMessage: %v", err)
	}
	raw := sender.buildMessage(msg, from, recipients)
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > smtpHeaderLineLimit {
			t.Fatalf("header line length = %d, want <= %d", len(line), smtpHeaderLineLimit)
		}
	}
	parsed, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message is not parseable: %v", err)
	}
	parsedTo, err := mail.ParseAddressList(parsed.Header.Get("To"))
	if err != nil {
		t.Fatalf("parse folded To header: %v", err)
	}
	if len(parsedTo) != len(to) {
		t.Fatalf("parsed %d recipients, want %d", len(parsedTo), len(to))
	}
}
