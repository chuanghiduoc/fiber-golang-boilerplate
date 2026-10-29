package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// sanitizeHeader drops control characters, which prevents SMTP header
// injection and keeps values within the header grammar. CR and LF are the
// injection vector; the rest are invalid inside a header field body.
func sanitizeHeader(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

const (
	// smtpTimeout bounds the TCP connect and, as a connection deadline, the whole
	// SMTP conversation.
	smtpTimeout = 30 * time.Second
	// smtpBodyLineWidth is the MIME base64 line limit from RFC 2045.
	smtpBodyLineWidth = 76
	// smtpHeaderLineLimit is the RFC 5322 hard limit for one header line,
	// excluding CRLF.
	smtpHeaderLineLimit = 998
	// smtpMaxRecipients bounds one SMTP envelope. A notification or
	// verification email never targets a large audience; accepting an
	// unbounded slice would let one call hold the conversation open.
	smtpMaxRecipients = 50
	// smtpEncodedWordBytes is the largest chunk of source text encoded into a
	// single RFC 2047 word. Q-encoding expands every non-ASCII byte to three
	// characters, so 20 bytes always stay below the 75-character word limit.
	smtpEncodedWordBytes = 20
)

// Send delivers msg in one SMTP conversation: connect, STARTTLS, AUTH,
// MAIL/RCPT, DATA, QUIT.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	from, recipients, err := s.prepareMessage(msg)
	if err != nil {
		return err
	}
	message := s.buildMessage(msg, from, recipients)

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
	if err := s.transmit(client, from.Address, recipientAddresses(recipients), message); err != nil {
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

// prepareMessage validates and normalizes the envelope before any network I/O.
// Header values are rebuilt from parsed addresses instead of copying raw input,
// which prevents malformed addresses and header injection.
func (s *SMTPSender) prepareMessage(msg Message) (from mail.Address, recipients []mail.Address, err error) {
	parsedFrom, err := mail.ParseAddress(strings.TrimSpace(s.from))
	if err != nil {
		return mail.Address{}, nil, fmt.Errorf("invalid sender address: %w", err)
	}
	from = *parsedFrom
	from.Name = s.fromName

	if len(msg.To) == 0 {
		return mail.Address{}, nil, fmt.Errorf("email message has no recipients")
	}
	if len(msg.To) > smtpMaxRecipients {
		return mail.Address{}, nil, fmt.Errorf("email message has %d recipients, maximum is %d", len(msg.To), smtpMaxRecipients)
	}
	recipients = make([]mail.Address, 0, len(msg.To))
	for _, raw := range msg.To {
		recipient, parseErr := mail.ParseAddress(strings.TrimSpace(raw))
		if parseErr != nil {
			return mail.Address{}, nil, fmt.Errorf("invalid recipient address: %w", parseErr)
		}
		recipients = append(recipients, *recipient)
	}
	return from, recipients, nil
}

func recipientAddresses(recipients []mail.Address) []string {
	out := make([]string, len(recipients))
	for i := range recipients {
		out[i] = recipients[i].Address
	}
	return out
}

// buildMessage renders the RFC 5322 headers and a 7-bit-safe body.
// Non-ASCII subjects and display names are RFC 2047 encoded; base64 also keeps
// long HTML lines within the SMTP line limit. A message that carries both a
// plain-text and an HTML body is sent as multipart/alternative so clients that
// cannot render HTML still receive the text instead of an empty message.
func (s *SMTPSender) buildMessage(msg Message, from mail.Address, recipients []mail.Address) string {
	to := make([]string, len(recipients))
	for i := range recipients {
		to[i] = formatAddr(recipients[i].Name, recipients[i].Address)
	}

	body := msg.Body
	contentType := "text/plain; charset=UTF-8"
	multipartBody := false
	if msg.HTML != "" {
		body = msg.HTML
		contentType = "text/html; charset=UTF-8"
		multipartBody = msg.Body != ""
	}

	headers := []mimeHeader{
		{"From", formatAddr(from.Name, from.Address)},
		{"To", foldAddressHeader(to)},
		{"Subject", encodeHeaderValue(msg.Subject)},
		{"Date", time.Now().UTC().Format(time.RFC1123Z)},
		{"MIME-Version", "1.0"},
	}
	if multipartBody {
		boundary := newMIMEBoundary()
		headers = append(headers, mimeHeader{"Content-Type", `multipart/alternative; boundary="` + boundary + `"`})
		return renderMIME(headers, renderMultipartAlternative(boundary, msg.Body, msg.HTML))
	}
	headers = append(headers,
		mimeHeader{"Content-Type", contentType},
		mimeHeader{"Content-Transfer-Encoding", "base64"},
	)
	return renderMIME(headers, encodeMIMEBase64(body))
}

type mimeHeader struct {
	name  string
	value string
}

// foldAddressHeader joins formatted addresses and folds the list so the To
// header stays inside the RFC 5322 line limit. A comma allows folding, so a
// long recipient list is split after a comma without changing its meaning.
func foldAddressHeader(addresses []string) string {
	const lineBudget = smtpHeaderLineLimit - len("To: ")
	var b strings.Builder
	lineLen := 0
	for i, addr := range addresses {
		if i > 0 {
			if lineLen+2+len(addr) > lineBudget {
				b.WriteString(",\r\n ")
				lineLen = 1
			} else {
				b.WriteString(", ")
				lineLen += 2
			}
		}
		b.WriteString(addr)
		lineLen += len(addr)
	}
	return b.String()
}
func renderMIME(headers []mimeHeader, body string) string {
	var message strings.Builder
	for _, header := range headers {
		fmt.Fprintf(&message, "%s: %s\r\n", header.name, header.value)
	}
	message.WriteString("\r\n")
	message.WriteString(body)
	return message.String()
}

func renderMultipartAlternative(boundary, textBody, htmlBody string) string {
	var message strings.Builder
	writePart := func(contentType, body string) {
		fmt.Fprintf(&message, "--%s\r\n", boundary)
		fmt.Fprintf(&message, "Content-Type: %s\r\n", contentType)
		message.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		message.WriteString(encodeMIMEBase64(body))
	}
	writePart("text/plain; charset=UTF-8", textBody)
	writePart("text/html; charset=UTF-8", htmlBody)
	fmt.Fprintf(&message, "--%s--\r\n", boundary)
	return message.String()
}

// newMIMEBoundary returns a random delimiter that cannot collide with the
// bodies with meaningful probability. crypto/rand failing is a broken system;
// a nanosecond timestamp still yields a syntactically valid boundary and does
// not affect message confidentiality or integrity.
func newMIMEBoundary() string {
	const prefix = "=_fiber_"
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s%x", prefix, raw)
}

// encodeHeaderValue RFC 2047-encodes non-ASCII (or very long) text and folds
// the encoded words into continuation lines. mime.QEncoding.Encode alone
// returns one unbounded word, which a long subject would push past the
// RFC 5322 line limit and some MTAs reject.
func encodeHeaderValue(value string) string {
	value = sanitizeHeader(value)
	if value == "" {
		return ""
	}
	if isPrintableASCII(value) && len("Subject: ")+len(value) <= smtpHeaderLineLimit {
		return value
	}
	var words []string
	var chunk strings.Builder
	chunkBytes := 0
	flush := func() {
		if chunk.Len() == 0 {
			return
		}
		words = append(words, encodeQWord(chunk.String()))
		chunk.Reset()
		chunkBytes = 0
	}
	for _, r := range value {
		size := utf8.RuneLen(r)
		if chunkBytes+size > smtpEncodedWordBytes {
			flush()
		}
		chunk.WriteRune(r)
		chunkBytes += size
	}
	flush()
	return strings.Join(words, "\r\n ")
}

// encodeQWord renders one RFC 2047 Q-encoded word. The standard library's
// mime.QEncoding.Encode returns pure-ASCII input unchanged, which would leave
// an unfolded literal chunk between encoded words; encoding every chunk here
// keeps the fold whitespace removable by decoders.
func encodeQWord(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(value)*3 + len("=?UTF-8?Q?") + len("?="))
	b.WriteString("=?UTF-8?Q?")
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == ' ':
			b.WriteByte('_')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		default:
			b.WriteByte('=')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	b.WriteString("?=")
	return b.String()
}

func isPrintableASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
func encodeMIMEBase64(value string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	if encoded == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(encoded) + len(encoded)/smtpBodyLineWidth*2 + 2)
	for len(encoded) > smtpBodyLineWidth {
		out.WriteString(encoded[:smtpBodyLineWidth])
		out.WriteString("\r\n")
		encoded = encoded[smtpBodyLineWidth:]
	}
	out.WriteString(encoded)
	out.WriteString("\r\n")
	return out.String()
}

// dial connects to the server with a context-aware dialer. smtp.Client has no
// context-aware methods, so a connection deadline plus a watcher that expires
// it on cancellation keeps a stalled server from leaking the connection or
// goroutine. The returned stop function ends the watcher.
func (s *SMTPSender) dial(ctx context.Context) (conn net.Conn, stop func(), err error) {
	dialer := net.Dialer{Timeout: smtpTimeout}
	conn, err = dialer.DialContext(ctx, "tcp", s.serverAddress())
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

func (s *SMTPSender) serverAddress() string {
	return net.JoinHostPort(s.host, strconv.Itoa(s.port))
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
func (s *SMTPSender) transmit(client *smtp.Client, from string, recipients []string, message string) error {
	if err := client.Mail(from); err != nil {
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
