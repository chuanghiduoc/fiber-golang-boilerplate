package authn

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// SecurityEvent identifies the account change a security notification reports.
// The value is also the template key suffix in the identity catalog.
type SecurityEvent string

const (
	SecurityEventPasswordChanged           SecurityEvent = "password_changed"
	SecurityEventPasswordReset             SecurityEvent = "password_reset"
	SecurityEventEmailChanged              SecurityEvent = "email_changed"
	SecurityEventMFAEnabled                SecurityEvent = "mfa_enabled"
	SecurityEventMFADisabled               SecurityEvent = "mfa_disabled"
	SecurityEventRecoveryCodesRegenerated  SecurityEvent = "recovery_codes_regenerated"
	SecurityEventMFAReset                  SecurityEvent = "mfa_reset"
	SecurityEventPasskeyAdded              SecurityEvent = "passkey_added"
	SecurityEventPasskeyRemoved            SecurityEvent = "passkey_removed"
	securityEmailLocaleFallback                          = "en"
	securityEmailPlaceholderUnavailableKey               = "identity.email.security.unknown"
)

// SecurityNotice describes one security-sensitive account change to email to
// the account owner. Email is the account's current (or new) address;
// PreviousEmail is set only for an email change, whose warning also goes to
// the old address. It never carries passwords, tokens, or recovery codes.
type SecurityNotice struct {
	Event         SecurityEvent
	UserID        uuid.UUID
	Name          string
	Email         string
	PreviousEmail string
	// PasskeyName labels a passkey_added/passkey_removed notice. It is never
	// included for other events.
	PasskeyName string
	OccurredAt  time.Time
}

// SecurityNotifier builds and delivers the localized security notification
// emails (OWASP ASVS V2.2.3/V6.3.7). Every message is enqueued on the same
// transaction as the account change, so it can never be sent before commit and
// an enqueue failure rolls the change back.
type SecurityNotifier struct {
	tenants TenantRepository
	now     func() time.Time
}

// NewSecurityNotifier wires the notifier. tenants resolves the tenant's email
// locale. Identity Build requires the transactional outbox, so there is no
// direct-send deployment mode.
func NewSecurityNotifier(tenants TenantRepository) *SecurityNotifier {
	return &SecurityNotifier{tenants: tenants, now: time.Now}
}

// Deliver enqueues the notice on the change's transaction. A missing outbox is
// a wiring error: sending directly would bypass the commit boundary, while
// silently dropping the notice would hide a security alert. A nil notifier is
// a no-op, which keeps minimal service wirings (unit tests) valid.
func (n *SecurityNotifier) Deliver(ctx context.Context, tx TxRepos, notice SecurityNotice) error {
	if n == nil {
		return nil
	}
	if tx.Mail == nil {
		return apperror.NewInternal("security notification requires the transactional email outbox")
	}
	messages, err := n.messages(ctx, tx, notice)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		if err := tx.Mail.Send(ctx, msg); err != nil {
			slog.Error("failed to enqueue security notification email",
				slog.String("user_id", notice.UserID.String()),
				slog.String("event", string(notice.Event)),
				slog.Any("error", err),
			)
			return apperror.NewInternal("failed to enqueue security notification email")
		}
	}
	return nil
}

func (n *SecurityNotifier) messages(ctx context.Context, tx TxRepos, notice SecurityNotice) ([]email.Message, error) {
	if !validSecurityEvent(notice.Event) {
		return nil, apperror.NewInternal("unknown security notification event")
	}
	if strings.TrimSpace(notice.Email) == "" {
		return nil, apperror.NewInternal("security notification has no recipient")
	}
	occurredAt := notice.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = n.now()
	}
	locale := n.locale(ctx, tx.Tenants)
	params, err := n.messageParams(ctx, locale, notice, occurredAt)
	if err != nil {
		return nil, err
	}
	subject, err := securityEmailText(locale, "identity.email.security.subject."+string(notice.Event), params)
	if err != nil {
		return nil, err
	}
	greeting, err := securityEmailText(locale, "identity.email.security.greeting", params)
	if err != nil {
		return nil, err
	}
	detail, err := securityEmailText(locale, "identity.email.security.detail."+string(notice.Event), params)
	if err != nil {
		return nil, err
	}
	previousDetail := ""
	if notice.Event == SecurityEventEmailChanged {
		// The old mailbox is told both addresses; the new mailbox is not told
		// the old address (a mistyped new address must not learn it).
		previousDetail, err = securityEmailText(locale, "identity.email.security.detail.email_changed_previous", params)
		if err != nil {
			return nil, err
		}
	}
	when, err := securityEmailText(locale, "identity.email.security.when", params)
	if err != nil {
		return nil, err
	}
	ipLine, err := securityEmailText(locale, "identity.email.security.ip", params)
	if err != nil {
		return nil, err
	}
	deviceLine, err := securityEmailText(locale, "identity.email.security.device", params)
	if err != nil {
		return nil, err
	}
	warning, err := securityEmailText(locale, "identity.email.security.warning", params)
	if err != nil {
		return nil, err
	}

	// One message per recipient: an email-change warning must not disclose the
	// new address to the old mailbox (or the reverse) through a shared To list.
	recipients := securityRecipients(notice)
	messages := make([]email.Message, 0, len(recipients))
	for _, recipient := range recipients {
		detailFor := detail
		if previousDetail != "" && !strings.EqualFold(notice.PreviousEmail, notice.Email) &&
			strings.EqualFold(recipient, notice.PreviousEmail) {
			detailFor = previousDetail
		}
		paragraphs := []string{greeting, detailFor, when, ipLine, deviceLine, warning}
		messages = append(messages, email.Message{
			To:      []string{recipient},
			Subject: subject,
			Body:    strings.Join(paragraphs, "\n\n"),
			HTML:    securityEmailHTML(paragraphs),
		})
	}
	return messages, nil
}

// messageParams resolves the template values, including the request metadata.
// The public reset route carries the caller's IP/user-agent on the context
// (WithClientIP/WithUserAgent) before the global audit middleware's
// RequestInfo exists; audit values win when present.
func (n *SecurityNotifier) messageParams(ctx context.Context, locale string, notice SecurityNotice, occurredAt time.Time) (map[string]any, error) {
	unknown, err := securityEmailText(locale, securityEmailPlaceholderUnavailableKey, nil)
	if err != nil {
		return nil, err
	}
	ip, userAgent := unknown, unknown
	if info := audit.RequestInfoFromContext(ctx); info != nil {
		if info.IP != "" {
			ip = info.IP
		}
		if info.UserAgent != "" {
			userAgent = info.UserAgent
		}
	}
	if ip == unknown {
		if value := clientIPFromContext(ctx); value != "" {
			ip = value
		}
	}
	if userAgent == unknown {
		if value := userAgentFromContext(ctx); value != "" {
			userAgent = value
		}
	}
	if notice.Event == SecurityEventMFAReset {
		// A platform reset is requested by an administrator: the account owner
		// must not receive the administrator's network or browser details.
		ip, userAgent = unknown, unknown
	}
	name := notice.Name
	if name == "" {
		name = notice.Email
	}
	return map[string]any{
		"name":           name,
		"email":          notice.Email,
		"previous_email": notice.PreviousEmail,
		"passkey_name":   notice.PasskeyName,
		"time":           occurredAt.UTC().Format("2006-01-02 15:04:05"),
		"ip":             ip,
		"user_agent":     userAgent,
	}, nil
}

// securityRecipients sends an email-change warning to the new address and to
// the old one, so the previous owner knows the address no longer belongs to the
// account. Every other notice goes to the account's current address only.
func securityRecipients(notice SecurityNotice) []string {
	recipients := []string{notice.Email}
	if notice.Event == SecurityEventEmailChanged && notice.PreviousEmail != "" &&
		!strings.EqualFold(notice.PreviousEmail, notice.Email) {
		recipients = append(recipients, notice.PreviousEmail)
	}
	return recipients
}

func securityEmailHTML(paragraphs []string) string {
	var out strings.Builder
	for _, paragraph := range paragraphs {
		out.WriteString("<p>")
		out.WriteString(strings.ReplaceAll(html.EscapeString(paragraph), "\n", "<br>"))
		out.WriteString("</p>")
	}
	return out.String()
}

func securityEmailText(locale, key string, params map[string]any) (string, error) {
	message, ok := identitycatalog.EmailText(locale, key, params)
	if !ok {
		return "", apperror.NewInternal(fmt.Sprintf("security notification template %q is missing in locale %q", key, locale))
	}
	return message, nil
}

// locale uses the request tenant's configured locale (tenant_settings.locale,
// documented as the language of tenant-facing email). A missing or invalid
// setting degrades to English rather than blocking a security change. The
// transaction-bound repository is preferred so the read joins the change's
// transaction instead of taking a second connection.
func (n *SecurityNotifier) locale(ctx context.Context, tenants TenantRepository) string {
	if tenants == nil {
		tenants = n.tenants
	}
	if tenants == nil {
		return securityEmailLocaleFallback
	}
	settings, err := tenants.Settings(ctx, tenant.IDFromContext(ctx))
	if err != nil {
		if !errors.Is(err, apperror.ErrNotFound) {
			slog.Warn("failed to resolve the tenant email locale", slog.Any("error", err))
		}
		return securityEmailLocaleFallback
	}
	if settings.Locale == "" {
		return securityEmailLocaleFallback
	}
	return settings.Locale
}

func validSecurityEvent(event SecurityEvent) bool {
	switch event {
	case SecurityEventPasswordChanged, SecurityEventPasswordReset, SecurityEventEmailChanged,
		SecurityEventMFAEnabled, SecurityEventMFADisabled, SecurityEventRecoveryCodesRegenerated,
		SecurityEventMFAReset, SecurityEventPasskeyAdded, SecurityEventPasskeyRemoved:
		return true
	default:
		return false
	}
}
