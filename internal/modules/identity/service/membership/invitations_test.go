package membership

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
	"github.com/google/uuid"
)

// recordingInvitations records the RLS scope each possession lookup runs
// under, and delegates storage to the in-memory store.
type recordingInvitations struct {
	db                 *fakes.MemDB
	tenantLookupBypass bool
	tenantLookupToken  []byte
	lockBypass         bool
	lockTenant         uuid.UUID
	lockToken          []byte
}

func (r *recordingInvitations) Upsert(ctx context.Context, in core.NewInvitation) (*core.Invitation, error) {
	return fakes.Invitations{DB: r.db}.Upsert(ctx, in)
}

func (r *recordingInvitations) ListPendingCursor(ctx context.Context, cur pagination.DBCursor) ([]core.Invitation, error) {
	return fakes.Invitations{DB: r.db}.ListPendingCursor(ctx, cur)
}

func (r *recordingInvitations) Get(ctx context.Context, id uuid.UUID) (*core.Invitation, error) {
	return fakes.Invitations{DB: r.db}.Get(ctx, id)
}

func (r *recordingInvitations) DeletePending(ctx context.Context, id uuid.UUID) (bool, error) {
	return fakes.Invitations{DB: r.db}.DeletePending(ctx, id)
}

func (r *recordingInvitations) TenantByTokenHash(ctx context.Context, tokenHash []byte) (uuid.UUID, error) {
	r.tenantLookupBypass = database.RLSBypass(ctx)
	r.tenantLookupToken = tokenHash
	return fakes.Invitations{DB: r.db}.TenantByTokenHash(ctx, tokenHash)
}

func (r *recordingInvitations) GetByTokenHashForUpdate(ctx context.Context, tenantID uuid.UUID, tokenHash []byte) (*core.Invitation, error) {
	r.lockBypass = database.RLSBypass(ctx)
	r.lockTenant = tenantID
	r.lockToken = tokenHash
	return fakes.Invitations{DB: r.db}.GetByTokenHashForUpdate(ctx, tenantID, tokenHash)
}

func (r *recordingInvitations) MarkAccepted(ctx context.Context, tenantID, id, userID uuid.UUID) (bool, error) {
	return fakes.Invitations{DB: r.db}.MarkAccepted(ctx, tenantID, id, userID)
}

type scopeCall struct {
	tenantID uuid.UUID
	bypass   bool
}

// scopedRecordingTx records the scope of each service transaction and binds
// the recording invitation repository into it.
type scopedRecordingTx struct {
	db          *fakes.MemDB
	invitations core.InvitationRepository
	calls       []scopeCall
}

func (t *scopedRecordingTx) InTx(ctx context.Context, fn func(context.Context, core.TxRepos) error) error {
	t.calls = append(t.calls, scopeCall{tenantID: tenant.IDFromContext(ctx), bypass: database.RLSBypass(ctx)})
	stores := t.db.Stores()
	stores.Invitations = t.invitations
	return fn(ctx, core.TxRepos{Stores: stores, Audit: fakes.Audit{DB: t.db}, Events: fakes.Events{DB: t.db}})
}

// TestInvitationAccept_OnlyTheTokenLookupRunsWithBypass pins the split: the
// possession-based tenant lookup bypasses RLS, and the membership/audit
// transaction runs under that tenant with an explicit tenant predicate.
func TestInvitationAccept_OnlyTheTokenLookupRunsWithBypass(t *testing.T) {
	db := fakes.New()
	now := time.Now().UTC()
	db.Mu.Lock()
	db.Users[fakes.TestID(1)] = &core.User{ID: fakes.TestID(1), Email: "inviter@example.com", Name: "Inviter", Status: core.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.Memberships[[2]uuid.UUID{fakes.TestID(2), fakes.TestID(1)}] = &core.Membership{
		ID: db.ID(), TenantID: fakes.TestID(2), UserID: fakes.TestID(1), Role: core.RoleOwner, Roles: []string{core.RoleOwner},
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.Users[fakes.TestID(2)] = &core.User{ID: fakes.TestID(2), Email: "invitee@example.com", Name: "Invitee", Status: core.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.Memberships[[2]uuid.UUID{fakes.TestID(1), fakes.TestID(2)}] = &core.Membership{
		ID: db.ID(), TenantID: fakes.TestID(1), UserID: fakes.TestID(2), Role: core.RoleMember, Roles: []string{core.RoleMember},
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.Mu.Unlock()
	if _, err := (fakes.Invitations{DB: db}).Upsert(tenant.WithID(context.Background(), fakes.TestID(2)), core.NewInvitation{
		Email: "invitee@example.com", Role: core.RoleMember, TokenHash: hashToken("invite-token"),
		InvitedBy: fakes.TestID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	invitations := &recordingInvitations{db: db}
	stores := db.Stores()
	stores.Invitations = invitations
	tx := &scopedRecordingTx{db: db, invitations: invitations}
	svc := NewInvitationService(stores, tx, "https://app.example.test")

	member, err := svc.Accept(context.Background(), fakes.TestID(2), "invite-token")
	if err != nil {
		t.Fatal(err)
	}
	if member.Membership.TenantID != fakes.TestID(2) {
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
	if invitations.lockTenant != fakes.TestID(2) {
		t.Errorf("invitation lock tenant = %s, want 2", invitations.lockTenant)
	}
	if len(tx.calls) != 1 {
		t.Fatalf("transactions = %d, want 1", len(tx.calls))
	}
	if tx.calls[0].bypass {
		t.Error("the membership/audit transaction must not bypass RLS")
	}
	if tx.calls[0].tenantID != fakes.TestID(2) {
		t.Errorf("transaction tenant = %s, want 2", tx.calls[0].tenantID)
	}
}

// The invitation's address is the authoritative email for the password
// policy. Before this check existed a sign-up accepted a password containing
// the invitee's own address, and it hashed the password before the token was
// resolved.
func TestInvitationSignUp_ValidatesPasswordAgainstInvitationEmail(t *testing.T) {
	db := fakes.New()
	now := time.Now().UTC()
	db.Mu.Lock()
	db.Users[fakes.TestID(1)] = &core.User{ID: fakes.TestID(1), Email: "owner@example.com", Name: "Owner", Status: core.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.Memberships[[2]uuid.UUID{fakes.TestID(1), fakes.TestID(1)}] = &core.Membership{
		ID: db.ID(), TenantID: fakes.TestID(1), UserID: fakes.TestID(1), Role: core.RoleOwner, Roles: []string{core.RoleOwner},
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.Mu.Unlock()
	if _, err := (fakes.Invitations{DB: db}).Upsert(tenant.WithID(context.Background(), fakes.TestID(1)), core.NewInvitation{
		Email: "signup.person@example.com", Role: core.RoleMember, TokenHash: hashToken("signup-token"),
		InvitedBy: fakes.TestID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewInvitationService(db.Stores(), fakes.Tx{DB: db}, "https://app.example.test")
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
	db.Mu.Lock()
	defer db.Mu.Unlock()
	for _, u := range db.Users {
		if u.Email == "signup.person@example.com" {
			t.Fatal("a rejected password must not create the account")
		}
	}
}

// A sign-up for a token from another tenant is refused before any transaction
// or write; only the possession lookup may bypass RLS.
func TestInvitationSignUp_RejectsAnotherTenantBeforeWriting(t *testing.T) {
	db := fakes.New()
	now := time.Now().UTC()
	db.Mu.Lock()
	db.Users[fakes.TestID(1)] = &core.User{ID: fakes.TestID(1), Email: "owner@example.com", Name: "Owner", Status: core.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	db.Memberships[[2]uuid.UUID{fakes.TestID(2), fakes.TestID(1)}] = &core.Membership{
		ID: db.ID(), TenantID: fakes.TestID(2), UserID: fakes.TestID(1), Role: core.RoleOwner, Roles: []string{core.RoleOwner},
		Status: core.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	db.Mu.Unlock()
	if _, err := (fakes.Invitations{DB: db}).Upsert(tenant.WithID(context.Background(), fakes.TestID(2)), core.NewInvitation{
		Email: "signup@example.com", Role: core.RoleMember, TokenHash: hashToken("signup-token"),
		InvitedBy: fakes.TestID(1), ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	invitations := &recordingInvitations{db: db}
	stores := db.Stores()
	stores.Invitations = invitations
	tx := &scopedRecordingTx{db: db, invitations: invitations}
	svc := NewInvitationService(stores, tx, "https://app.example.test")

	_, err := svc.SignUp(tenant.WithID(context.Background(), tenant.DefaultID), InvitationSignUp{
		Token: "signup-token", Name: "Signup", Password: fakes.Passphrase,
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
	db.Mu.Lock()
	defer db.Mu.Unlock()
	for _, u := range db.Users {
		if u.Email == "signup@example.com" {
			t.Fatal("a refused sign-up must not create the account")
		}
	}
}
