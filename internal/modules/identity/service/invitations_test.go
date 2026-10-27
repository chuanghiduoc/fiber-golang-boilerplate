package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
	"github.com/google/uuid"
)

// recordingInvitations records the RLS scope each possession lookup runs
// under, and delegates storage to the in-memory store.
type recordingInvitations struct {
	db                 *memDB
	tenantLookupBypass bool
	tenantLookupToken  []byte
	lockBypass         bool
	lockTenant         uuid.UUID
	lockToken          []byte
}

func (r *recordingInvitations) Upsert(ctx context.Context, in NewInvitation) (*Invitation, error) {
	return memInvitations{r.db}.Upsert(ctx, in)
}

func (r *recordingInvitations) ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]Invitation, error) {
	return memInvitations{r.db}.ListPendingCursor(ctx, cur)
}

func (r *recordingInvitations) Get(ctx context.Context, id uuid.UUID) (*Invitation, error) {
	return memInvitations{r.db}.Get(ctx, id)
}

func (r *recordingInvitations) DeletePending(ctx context.Context, id uuid.UUID) (bool, error) {
	return memInvitations{r.db}.DeletePending(ctx, id)
}

func (r *recordingInvitations) TenantByTokenHash(ctx context.Context, tokenHash []byte) (uuid.UUID, error) {
	r.tenantLookupBypass = database.RLSBypass(ctx)
	r.tenantLookupToken = tokenHash
	return memInvitations{r.db}.TenantByTokenHash(ctx, tokenHash)
}

func (r *recordingInvitations) GetByTokenHashForUpdate(ctx context.Context, tenantID uuid.UUID, tokenHash []byte) (*Invitation, error) {
	r.lockBypass = database.RLSBypass(ctx)
	r.lockTenant = tenantID
	r.lockToken = tokenHash
	return memInvitations{r.db}.GetByTokenHashForUpdate(ctx, tenantID, tokenHash)
}

func (r *recordingInvitations) MarkAccepted(ctx context.Context, tenantID, id, userID uuid.UUID) (bool, error) {
	return memInvitations{r.db}.MarkAccepted(ctx, tenantID, id, userID)
}

type scopeCall struct {
	tenantID uuid.UUID
	bypass   bool
}

// scopedRecordingTx records the scope of each service transaction and binds
// the recording invitation repository into it.
type scopedRecordingTx struct {
	db          *memDB
	invitations InvitationRepository
	calls       []scopeCall
}

func (t *scopedRecordingTx) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	t.calls = append(t.calls, scopeCall{tenantID: tenant.IDFromContext(ctx), bypass: database.RLSBypass(ctx)})
	stores := t.db.stores()
	stores.Invitations = t.invitations
	return fn(ctx, TxRepos{Stores: stores, Audit: memAudit{db: t.db}, Events: memEvents{db: t.db}})
}

// TestInvitationAccept_OnlyTheTokenLookupRunsWithBypass pins the split: the
// possession-based tenant lookup bypasses RLS, and the membership/audit
// transaction runs under that tenant with an explicit tenant predicate.
func TestInvitationAccept_OnlyTheTokenLookupRunsWithBypass(t *testing.T) {
	db := newMemDB()
	now := time.Now().UTC()
	db.mu.Lock()
	db.users[testID(1)] = &User{ID: testID(1), Email: "inviter@example.com", Name: "Inviter", Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.memberships[[2]uuid.UUID{testID(2), testID(1)}] = &Membership{
		ID: db.id(), TenantID: testID(2), UserID: testID(1), Role: RoleOwner, Roles: []string{RoleOwner},
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.users[testID(2)] = &User{ID: testID(2), Email: "invitee@example.com", Name: "Invitee", Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.memberships[[2]uuid.UUID{testID(1), testID(2)}] = &Membership{
		ID: db.id(), TenantID: testID(1), UserID: testID(2), Role: RoleMember, Roles: []string{RoleMember},
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.mu.Unlock()
	if _, err := (memInvitations{db}).Upsert(tenant.WithID(context.Background(), testID(2)), NewInvitation{
		Email: "invitee@example.com", Role: RoleMember, TokenHash: hashToken("invite-token"),
		InvitedBy: testID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	invitations := &recordingInvitations{db: db}
	stores := db.stores()
	stores.Invitations = invitations
	tx := &scopedRecordingTx{db: db, invitations: invitations}
	svc := NewInvitationService(stores, tx, "https://app.example.test")

	member, err := svc.Accept(context.Background(), testID(2), "invite-token")
	if err != nil {
		t.Fatal(err)
	}
	if member.Membership.TenantID != testID(2) {
		t.Fatalf("joined tenant %s, want 2", member.Membership.TenantID)
	}
	if !invitations.tenantLookupBypass {
		t.Error("the possession-based tenant lookup must run with the RLS bypass")
	}
	if !bytes.Equal(invitations.tenantLookupToken, hashToken("invite-token")) {
		t.Error("the tenant lookup must hash the token")
	}
	if invitations.lockBypass {
		t.Error("the invitation lock must run under the tenant-scoped transaction")
	}
	if invitations.lockTenant != testID(2) {
		t.Errorf("invitation lock tenant = %s, want 2", invitations.lockTenant)
	}
	if len(tx.calls) != 1 {
		t.Fatalf("transactions = %d, want 1", len(tx.calls))
	}
	if tx.calls[0].bypass {
		t.Error("the membership/audit transaction must not bypass RLS")
	}
	if tx.calls[0].tenantID != testID(2) {
		t.Errorf("transaction tenant = %s, want 2", tx.calls[0].tenantID)
	}
}

// The invitation's address is the authoritative email for the password
// policy. Before this check existed a sign-up accepted a password containing
// the invitee's own address, and it hashed the password before the token was
// resolved.
func TestInvitationSignUp_ValidatesPasswordAgainstInvitationEmail(t *testing.T) {
	db := newMemDB()
	now := time.Now().UTC()
	db.mu.Lock()
	db.users[testID(1)] = &User{ID: testID(1), Email: "owner@example.com", Name: "Owner", Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.memberships[[2]uuid.UUID{testID(1), testID(1)}] = &Membership{
		ID: db.id(), TenantID: testID(1), UserID: testID(1), Role: RoleOwner, Roles: []string{RoleOwner},
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.mu.Unlock()
	if _, err := (memInvitations{db}).Upsert(tenant.WithID(context.Background(), testID(1)), NewInvitation{
		Email: "signup.person@example.com", Role: RoleMember, TokenHash: hashToken("signup-token"),
		InvitedBy: testID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewInvitationService(db.stores(), memTx{db}, "https://app.example.test")
	_, err := svc.SignUp(tenant.WithID(context.Background(), tenant.DefaultID), InvitationSignUp{
		Token: "signup-token", Name: "Signup", Password: fmt.Sprintf("%s-%d", "signup.person", 2026),
	})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("SignUp error = %v, want a validation error", err)
	}
	if resolvedStatus(appErr) != 422 || len(appErr.Errors) == 0 {
		t.Fatalf("SignUp error = %v, want 422 with a field error", err)
	}
	if got := appErr.Errors[0].Code; got != validator.CodePasswordFromEmail {
		t.Errorf("field code = %q, want %q", got, validator.CodePasswordFromEmail)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, u := range db.users {
		if u.Email == "signup.person@example.com" {
			t.Fatal("a rejected password must not create the account")
		}
	}
}

// A sign-up for a token from another tenant is refused before any transaction
// or write; only the possession lookup may bypass RLS.
func TestInvitationSignUp_RejectsAnotherTenantBeforeWriting(t *testing.T) {
	db := newMemDB()
	now := time.Now().UTC()
	db.mu.Lock()
	db.users[testID(1)] = &User{ID: testID(1), Email: "owner@example.com", Name: "Owner", Status: UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.memberships[[2]uuid.UUID{testID(2), testID(1)}] = &Membership{
		ID: db.id(), TenantID: testID(2), UserID: testID(1), Role: RoleOwner, Roles: []string{RoleOwner},
		Status: MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.mu.Unlock()
	if _, err := (memInvitations{db}).Upsert(tenant.WithID(context.Background(), testID(2)), NewInvitation{
		Email: "signup@example.com", Role: RoleMember, TokenHash: hashToken("signup-token"),
		InvitedBy: testID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	invitations := &recordingInvitations{db: db}
	stores := db.stores()
	stores.Invitations = invitations
	tx := &scopedRecordingTx{db: db, invitations: invitations}
	svc := NewInvitationService(stores, tx, "https://app.example.test")

	_, err := svc.SignUp(tenant.WithID(context.Background(), tenant.DefaultID), InvitationSignUp{
		Token: "signup-token", Name: "Signup", Password: testPassphrase,
	})
	if err == nil {
		t.Fatal("expected a token from another tenant to be refused")
	}
	if !invitations.tenantLookupBypass {
		t.Error("the possession-based tenant lookup must run with the RLS bypass")
	}
	if len(tx.calls) != 0 {
		t.Error("a mismatched sign-up must not start a transaction")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, u := range db.users {
		if u.Email == "signup@example.com" {
			t.Fatal("a refused sign-up must not create the account")
		}
	}
}
