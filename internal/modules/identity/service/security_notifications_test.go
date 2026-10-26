package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

// captureEmails records notifications; it implements both service.Mailer (the
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
func notifierFor(db *memDB, sender email.Sender) *SecurityNotifier {
	return NewSecurityNotifier(db.stores().Tenants, sender)
}

func seedNotifiableUser(t *testing.T, db *memDB, id int64, emailAddr string) *Member {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	now := time.Now().UTC()
	db.mu.Lock()
	defer db.mu.Unlock()
	db.users[id] = &User{ID: id, Email: emailAddr, Name: "Notify User", Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.passwords[id] = string(hash)
	db.memberships[[2]int64{tenant.DefaultID, id}] = &Membership{
		ID: db.id(), TenantID: tenant.DefaultID, UserID: id, Role: RoleMember,
		Roles: []string{RoleMember}, Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	return &Member{User: *db.users[id], Membership: *db.memberships[[2]int64{tenant.DefaultID, id}]}
}

// The notifier localizes every event, includes the UTC time and request
// metadata, and sends the email-change warning to both addresses.
func TestSecurityNotifications_BuildLocalizedEmails(t *testing.T) {
	db := newMemDB()
	mail := &captureEmails{}
	db.mail = mail
	n := notifierFor(db, nil)
	ctx := audit.WithRequestMeta(context.Background(), "req-1", "203.0.113.9", "TestBrowser/1.0")
	at := time.Date(2026, 10, 26, 9, 30, 0, 0, time.UTC)

	for _, event := range []SecurityEvent{
		SecurityEventPasswordChanged,
		SecurityEventPasswordReset,
		SecurityEventEmailChanged,
		SecurityEventMFAEnabled,
		SecurityEventMFADisabled,
		SecurityEventRecoveryCodesRegenerated,
		SecurityEventMFAReset,
	} {
		mail.msgs = nil
		err := n.Deliver(ctx, TxRepos{Mail: mail}, SecurityNotice{
			Event: event, UserID: 7, Name: "Nguyen Van A", Email: "new@example.com",
			PreviousEmail: "old@example.com", OccurredAt: at,
		})
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		msgs := mail.messages()
		wantMessages := 1
		if event == SecurityEventEmailChanged {
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
		if event == SecurityEventMFAReset {
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
		if event == SecurityEventEmailChanged {
			if msgs[0].To[0] != "new@example.com" || msgs[1].To[0] != "old@example.com" {
				t.Errorf("email change recipients = %v / %v", msgs[0].To, msgs[1].To)
			}
		}
	}
}

func TestSecurityNotifications_UsesTenantLocale(t *testing.T) {
	db := newMemDB()
	db.locale = "vi"
	mail := &captureEmails{}
	db.mail = mail
	n := notifierFor(db, nil)

	if err := n.Deliver(context.Background(), TxRepos{Mail: mail}, SecurityNotice{
		Event: SecurityEventPasswordChanged, UserID: 1, Name: "A", Email: "a@example.com",
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
	db := newMemDB()
	sender := &captureEmails{}
	n := notifierFor(db, sender)

	var afterCommit func()
	if err := n.Deliver(context.Background(), TxRepos{AfterCommit: func(f func()) { afterCommit = f }}, SecurityNotice{
		Event: SecurityEventPasswordChanged, UserID: 1, Name: "A", Email: "a@example.com",
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
	db := newMemDB()
	mail := &captureEmails{fail: context.DeadlineExceeded}
	n := notifierFor(db, nil)
	if err := n.Deliver(context.Background(), TxRepos{Mail: mail}, SecurityNotice{
		Event: SecurityEventPasswordChanged, UserID: 1, Name: "A", Email: "a@example.com",
	}); err == nil {
		t.Fatal("expected the enqueue failure to surface")
	}
	if mail.count() != 0 {
		t.Fatal("a failed enqueue must not record a message")
	}
}

func TestChangePassword_EnqueuesSecurityNotification(t *testing.T) {
	db := newMemDB()
	mail := &captureEmails{}
	db.mail = mail
	seedNotifiableUser(t, db, 10, "change@example.com")
	users := NewUserService(UserDeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	if err := users.ChangePassword(context.Background(), 10, ChangePasswordInput{
		CurrentPassword: "Password1!", NewPassword: "Password2!",
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
	if strings.Contains(msgs[0].Body, "Password2!") {
		t.Error("the notification leaked the new password")
	}

	before := mail.count()
	if err := users.ChangePassword(context.Background(), 10, ChangePasswordInput{
		CurrentPassword: "wrong-password", NewPassword: "Password3!",
	}); err == nil {
		t.Fatal("expected a wrong current password to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed password change must not enqueue a notification")
	}
}

func TestResetPassword_EnqueuesSecurityNotification(t *testing.T) {
	db := newMemDB()
	mail := &captureEmails{}
	db.mail = mail
	seedNotifiableUser(t, db, 11, "reset-notify@example.com")
	stores := db.stores()
	if created, err := stores.Resets.Create(context.Background(), 11, "reset-notify@example.com", hashToken("reset-token"), time.Now().Add(time.Hour)); err != nil || !created {
		t.Fatalf("create reset token: created=%v err=%v", created, err)
	}
	resets := NewPasswordResetService(ResetDeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	ctx := WithUserAgent(WithClientIP(context.Background(), "198.51.100.7"), "ResetBrowser/2.0")
	if err := resets.ResetPassword(ctx, ResetPasswordInput{Token: "reset-token", Password: "Password2@"}); err != nil {
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
	if err := resets.ResetPassword(ctx, ResetPasswordInput{Token: "reset-token", Password: "Password3#"}); err == nil {
		t.Fatal("expected the consumed token to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed reset must not enqueue a notification")
	}
}

func TestEmailChange_NotifiesOldAndNewAddresses(t *testing.T) {
	db := newMemDB()
	mail := &captureEmails{}
	db.mail = mail
	seedNotifiableUser(t, db, 12, "old-address@example.com")
	users := NewUserService(UserDeps{
		Stores: db.stores(), Tx: memTx{db}, Cache: cache.NewMemoryCache(),
		Security: notifierFor(db, nil),
	})

	name := "Renamed"
	if _, err := users.Update(context.Background(), 12, UpdateProfileInput{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if mail.count() != 0 {
		t.Fatal("a name-only change must not send a security notification")
	}

	newAddress := "new-address@example.com"
	if _, err := users.Update(context.Background(), 12, UpdateProfileInput{Email: &newAddress}); err != nil {
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

	seedNotifiableUser(t, db, 13, "taken@example.com")
	before := mail.count()
	taken := "taken@example.com"
	if _, err := users.Update(context.Background(), 12, UpdateProfileInput{Email: &taken}); err == nil {
		t.Fatal("expected an address already in use to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed email change must not enqueue a notification")
	}
}

func TestMFAChanges_EnqueueSecurityNotifications(t *testing.T) {
	h := newMFAHarness(t)
	ctx := context.Background()
	mail := &captureEmails{}
	h.db.mail = mail
	h.svc = NewMFAService(MFADeps{
		Stores: h.db.stores(), Tx: memTx{h.db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: h.sessions, Now: h.clock.now,
		Security: notifierFor(h.db, nil),
	})

	secret, codes := h.enrollAndConfirm(t)
	if msgs := mail.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "two-factor authentication was enabled") {
		t.Fatalf("confirm notifications = %+v", msgs)
	}

	h.clock.at = h.clock.at.Add(totp.Period)
	if _, err := h.svc.RegenerateRecoveryCodes(ctx, 100, codes[0]); err != nil {
		t.Fatal(err)
	}
	if msgs := mail.messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Subject, "recovery codes were regenerated") {
		t.Fatalf("regenerate notifications = %+v", msgs)
	}

	h.clock.at = h.clock.at.Add(totp.Period)
	code, _ := totp.Code(secret, h.clock.at)
	if err := h.svc.Disable(ctx, 100, 1, code); err != nil {
		t.Fatal(err)
	}
	if msgs := mail.messages(); len(msgs) != 3 || !strings.Contains(msgs[2].Subject, "two-factor authentication was disabled") {
		t.Fatalf("disable notifications = %+v", msgs)
	}

	before := mail.count()
	if err := h.svc.Disable(ctx, 100, 1, code); err == nil {
		t.Fatal("expected disabling without a factor to fail")
	}
	if mail.count() != before {
		t.Fatal("a failed MFA change must not enqueue a notification")
	}

	reset := newMFAHarness(t)
	resetMail := &captureEmails{}
	reset.db.mail = resetMail
	reset.svc = NewMFAService(MFADeps{
		Stores: reset.db.stores(), Tx: memTx{reset.db}, Cache: cache.NewMemoryCache(),
		Sealer: reversibleSealer{}, Sessions: reset.sessions, Now: reset.clock.now,
		Security: notifierFor(reset.db, nil),
	})
	if err := reset.svc.ResetTOTP(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if msgs := resetMail.messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "two-factor authentication was reset") {
		t.Fatalf("platform reset notifications = %+v", msgs)
	}
}
