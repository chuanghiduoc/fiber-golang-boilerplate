package mfa

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
	"github.com/google/uuid"
)

// captureEmails records notifications; it implements both core.Mailer (the
// transactional outbox) and email.Sender (the after-commit fallback).
type captureEmails struct {
	mu   sync.Mutex
	msgs []email.Message
	fail error
}

func (c *captureEmails) Send(_ context.Context, msg email.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.msgs = append(c.msgs, msg)
	return nil
}

func (c *captureEmails) messages() []email.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]email.Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func (c *captureEmails) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// notifierFor builds a notifier over the fake tenant settings and the
// transaction's mail outbox.
func notifierFor(db *fakes.MemDB, sender email.Sender) *authn.SecurityNotifier {
	return authn.NewSecurityNotifier(db.Stores().Tenants, sender)
}

func seedNotifiableUser(t *testing.T, db *fakes.MemDB, id uuid.UUID, emailAddr string) *core.Member {
	t.Helper()
	hash, err := core.HashPassword(fakes.Passphrase)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	now := time.Now().UTC()
	db.Mu.Lock()
	defer db.Mu.Unlock()
	db.Users[id] = &core.User{ID: id, Email: emailAddr, Name: "Notify User", Status: core.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.Passwords[id] = hash
	db.Memberships[[2]uuid.UUID{tenant.DefaultID, id}] = &core.Membership{
		ID: db.ID(), TenantID: tenant.DefaultID, UserID: id, Role: core.RoleMember,
		Roles: []string{core.RoleMember}, Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	return &core.Member{User: *db.Users[id], Membership: *db.Memberships[[2]uuid.UUID{tenant.DefaultID, id}]}
}

// The notifier localizes every event, includes the UTC time and request
// metadata, and sends the email-change warning to both addresses.
func TestSecurityNotifications_BuildLocalizedEmails(t *testing.T) {
	db := fakes.New()
	mail := &captureEmails{}
	db.Mail = mail
	n := notifierFor(db, nil)
	ctx := audit.WithRequestMeta(fakes.TenantCtx(), "req-1", "203.0.113.9", "TestBrowser/1.0")
	at := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)

	for _, event := range []authn.SecurityEvent{
		authn.SecurityEventPasswordChanged,
		authn.SecurityEventPasswordReset,
		authn.SecurityEventEmailChanged,
		authn.SecurityEventMFAEnabled,
		authn.SecurityEventMFADisabled,
		authn.SecurityEventRecoveryCodesRegenerated,
		authn.SecurityEventMFAReset,
	} {
		mail.msgs = nil
		err := n.Deliver(ctx, core.TxRepos{Mail: mail}, authn.SecurityNotice{
			Event: event, UserID: fakes.TestID(7), Name: "Nguyen Van A", Email: "new@example.com",
			PreviousEmail: "old@example.com", OccurredAt: at,
		})
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		msgs := mail.messages()
		wantMessages := 1
		if event == authn.SecurityEventEmailChanged {
			wantMessages = 2
		}
		if len(msgs) != wantMessages {
			t.Fatalf("%s: %d messages, want %d", event, len(msgs), wantMessages)
		}
		if !strings.Contains(msgs[0].Subject, "Security alert") {
			t.Errorf("%s: subject %q is not a security alert", event, msgs[0].Subject)
		}
		for _, want := range []string{"2026-10-26 09:30:00 UTC", "If this wasn't you"} {
			if !strings.Contains(msgs[0].Body, want) {
				t.Errorf("%s: body missing %q:\n%s", event, want, msgs[0].Body)
			}
		}
		if event == authn.SecurityEventMFAReset {
			if strings.Contains(msgs[0].Body, "203.0.113.9") || strings.Contains(msgs[0].Body, "TestBrowser/1.0") {
				t.Errorf("a platform reset must not disclose the administrator's metadata:\n%s", msgs[0].Body)
			}
		} else {
			for _, want := range []string{"203.0.113.9", "TestBrowser/1.0"} {
				if !strings.Contains(msgs[0].Body, want) {
					t.Errorf("%s: body missing %q:\n%s", event, want, msgs[0].Body)
				}
			}
		}
		if event == authn.SecurityEventEmailChanged {
			if msgs[0].To[0] != "new@example.com" || msgs[1].To[0] != "old@example.com" {
				t.Errorf("email change recipients = %v / %v", msgs[0].To, msgs[1].To)
			}
		}
	}
}

func TestSecurityNotifications_UsesTenantLocale(t *testing.T) {
	db := fakes.New()
	db.Locale = "vi"
	mail := &captureEmails{}
	db.Mail = mail
	n := notifierFor(db, nil)

	if err := n.Deliver(fakes.TenantCtx(), core.TxRepos{Mail: mail}, authn.SecurityNotice{
		Event: authn.SecurityEventPasswordChanged, UserID: fakes.TestID(1), Name: "A", Email: "a@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	msgs := mail.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "Cảnh báo bảo mật") {
		t.Fatalf("notification is not localized: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Body, "không phải bạn") {
		t.Errorf("warning is not localized:\n%s", msgs[0].Body)
	}
}

// Without a transactional outbox the notifier never sends before the caller
// commits; it hands back the post-commit function instead.
func TestSecurityNotifications_AfterCommitFallback(t *testing.T) {
	db := fakes.New()
	sender := &captureEmails{}
	n := notifierFor(db, sender)

	var afterCommit func()
	if err := n.Deliver(fakes.TenantCtx(), core.TxRepos{AfterCommit: func(f func()) { afterCommit = f }}, authn.SecurityNotice{
		Event: authn.SecurityEventPasswordChanged, UserID: fakes.TestID(1), Name: "A", Email: "a@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 0 {
		t.Fatal("notification was sent before commit")
	}
	if afterCommit == nil {
		t.Fatal("expected an after-commit sender")
	}
	afterCommit()
	if sender.count() != 1 {
		t.Fatalf("after-commit messages = %d, want 1", sender.count())
	}
}

// An enqueue failure aborts the change (the caller turns the error into a 500
// and the transaction rolls back), so no notification is recorded.
func TestSecurityNotifications_EnqueueFailureAborts(t *testing.T) {
	db := fakes.New()
	mail := &captureEmails{fail: context.DeadlineExceeded}
	n := notifierFor(db, nil)
	if err := n.Deliver(fakes.TenantCtx(), core.TxRepos{Mail: mail}, authn.SecurityNotice{
		Event: authn.SecurityEventPasswordChanged, UserID: fakes.TestID(1), Name: "A", Email: "a@example.com",
	}); err == nil {
		t.Fatal("expected the enqueue failure to surface")
	}
	if mail.count() != 0 {
		t.Fatal("a failed enqueue must not record a message")
	}
}

func TestChangePassword_EnqueuesSecurityNotification(t *testing.T) {
	db := fakes.New()
	mail := &captureEmails{}
	db.Mail = mail
	seedNotifiableUser(t, db, fakes.TestID(10), "change@example.com")
	users := authn.NewUserService(authn.UserDeps{
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	if err := users.ChangePassword(fakes.TenantCtx(), fakes.TestID(10), authn.ChangePasswordInput{
		CurrentPassword: fakes.Passphrase, NewPassword: fakes.NewPassword,
	}); err != nil {
		t.Fatal(err)
	}
	msgs := mail.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Subject, "password was changed") || msgs[0].To[0] != "change@example.com" {
		t.Fatalf("unexpected notification: %+v", msgs[0])
	}
	if strings.Contains(msgs[0].Body, fakes.NewPassword) {
		t.Error("the notification leaked the new password")
	}

	before := mail.count()
	if err := users.ChangePassword(fakes.TenantCtx(), fakes.TestID(10), authn.ChangePasswordInput{
		CurrentPassword: fakes.WrongPassword, NewPassword: fakes.AnotherPassword,
	}); err == nil {
		t.Fatal("expected a wrong current password to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed password change must not enqueue a notification")
	}
}

func TestResetPassword_EnqueuesSecurityNotification(t *testing.T) {
	db := fakes.New()
	mail := &captureEmails{}
	db.Mail = mail
	seedNotifiableUser(t, db, fakes.TestID(11), "reset-notify@example.com")
	stores := db.Stores()
	if created, err := stores.Resets.Create(fakes.TenantCtx(), fakes.TestID(11), "reset-notify@example.com", hashToken("reset-token"), time.Now().Add(time.Hour)); err != nil || !created {
		t.Fatalf("create reset token: created=%v err=%v", created, err)
	}
	resets := authn.NewPasswordResetService(authn.ResetDeps{
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	ctx := core.WithUserAgent(core.WithClientIP(fakes.TenantCtx(), "198.51.100.7"), "ResetBrowser/2.0")
	if err := resets.ResetPassword(ctx, authn.ResetPasswordInput{Token: "reset-token", Password: fakes.NewPassword}); err != nil {
		t.Fatal(err)
	}
	msgs := mail.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "password was reset") {
		t.Fatalf("unexpected notifications: %+v", msgs)
	}
	for _, want := range []string{"198.51.100.7", "ResetBrowser/2.0"} {
		if !strings.Contains(msgs[0].Body, want) {
			t.Errorf("reset notification is missing request metadata %q:\n%s", want, msgs[0].Body)
		}
	}

	before := mail.count()
	if err := resets.ResetPassword(ctx, authn.ResetPasswordInput{Token: "reset-token", Password: fakes.AnotherPassword}); err == nil {
		t.Fatal("expected the consumed token to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed reset must not enqueue a notification")
	}
}

func TestEmailChange_NotifiesOldAndNewAddresses(t *testing.T) {
	db := fakes.New()
	mail := &captureEmails{}
	db.Mail = mail
	seedNotifiableUser(t, db, fakes.TestID(12), "old-address@example.com")
	users := authn.NewUserService(authn.UserDeps{
		Stores: db.Stores(), Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	name := "Renamed"
	if _, err := users.Update(fakes.TenantCtx(), fakes.TestID(12), authn.UpdateProfileInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if mail.count() != 0 {
		t.Fatal("a name-only change must not send a security notification")
	}

	newAddress := "new-address@example.com"
	if _, err := users.Update(fakes.TenantCtx(), fakes.TestID(12), authn.UpdateProfileInput{Email: &newAddress}); err != nil {
		t.Fatal(err)
	}
	msgs := mail.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want one per address", len(msgs))
	}
	if msgs[0].To[0] != newAddress || msgs[1].To[0] != "old-address@example.com" {
		t.Fatalf("recipients = %v / %v, want the new and old address", msgs[0].To, msgs[1].To)
	}
	if strings.Contains(msgs[0].Body, "old-address@example.com") {
		t.Errorf("the new address must not learn the previous address:\n%s", msgs[0].Body)
	}
	if !strings.Contains(msgs[1].Body, "old-address@example.com") || !strings.Contains(msgs[1].Body, newAddress) {
		t.Fatalf("the old address must be told both addresses:\n%s", msgs[1].Body)
	}

	seedNotifiableUser(t, db, fakes.TestID(13), "taken@example.com")
	before := mail.count()
	taken := "taken@example.com"
	if _, err := users.Update(fakes.TenantCtx(), fakes.TestID(12), authn.UpdateProfileInput{Email: &taken}); err == nil {
		t.Fatal("expected an address already in use to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed email change must not enqueue a notification")
	}
}

func TestMFAChanges_EnqueueSecurityNotifications(t *testing.T) {
	h := newMFAHarness(t)
	ctx := fakes.TenantCtx()
	mail := &captureEmails{}
	h.db.Mail = mail
	h.svc = NewMFAService(MFADeps{
		Stores: h.db.Stores(), Tx: fakes.Tx{DB: h.db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
		Security: notifierFor(h.db, nil),
	})

	secret, codes := h.enrollAndConfirm(t)
	if msgs := mail.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "two-factor authentication was enabled") {
		t.Fatalf("confirm notifications = %+v", msgs)
	}

	h.clock.at = h.clock.at.Add(totp.Period)
	if _, err := h.svc.RegenerateRecoveryCodes(ctx, fakes.TestID(100), codes[0]); err != nil {
		t.Fatal(err)
	}
	if msgs := mail.messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Subject, "recovery codes were regenerated") {
		t.Fatalf("regenerate notifications = %+v", msgs)
	}

	h.clock.at = h.clock.at.Add(totp.Period)
	code, _ := totp.Code(secret, h.clock.at)
	if err := h.svc.Disable(ctx, fakes.TestID(100), fakes.TestID(1), code); err != nil {
		t.Fatal(err)
	}
	if msgs := mail.messages(); len(msgs) != 3 || !strings.Contains(msgs[2].Subject, "two-factor authentication was disabled") {
		t.Fatalf("disable notifications = %+v", msgs)
	}

	before := mail.count()
	if err := h.svc.Disable(ctx, fakes.TestID(100), fakes.TestID(1), code); err == nil {
		t.Fatal("expected disabling without a factor to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed MFA change must not enqueue a notification")
	}

	reset := newMFAHarness(t)
	resetMail := &captureEmails{}
	reset.db.Mail = resetMail
	reset.svc = NewMFAService(MFADeps{
		Stores: reset.db.Stores(), Tx: fakes.Tx{DB: reset.db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: reset.sessions, Now: reset.clock.now,
		Security: notifierFor(reset.db, nil),
	})
	if err := reset.svc.ResetTOTP(ctx, fakes.TestID(100)); err != nil {
		t.Fatal(err)
	}
	if msgs := resetMail.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "two-factor authentication was reset") {
		t.Fatalf("platform reset notifications = %+v", msgs)
	}
}
