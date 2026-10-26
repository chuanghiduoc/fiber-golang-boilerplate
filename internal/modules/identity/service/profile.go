package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// UpdateProfileInput changes the caller's global profile. Email is only
// applied when set.
type UpdateProfileInput struct {
	Name  *string
	Email *string
}

// profileChange is a validated profile update: the values to store and what
// actually changes.
type profileChange struct {
	name         string
	email        string
	updateEmail  bool // the request supplied an email (possibly unchanged)
	nameChanged  bool
	emailChanged bool
}

func (c profileChange) auditMetadata() map[string]any {
	return map[string]any{
		"email_changed": c.emailChanged,
		"name_changed":  c.nameChanged,
	}
}

// Update changes the global profile of an active member of the request
// tenant. The handler only allows a user to change their own profile: a
// tenant admin manages memberships, never another person's global identity.
func (s *userService) Update(ctx context.Context, id int64, req UpdateProfileInput) (*Member, error) {
	existing, err := getMember(ctx, s.Stores.Memberships, id)
	if err != nil {
		return nil, err
	}
	change, err := s.planProfileChange(ctx, &existing.User, req)
	if err != nil {
		return nil, err
	}

	var updated *User
	// The email UPDATE also bumps the token version, which ends every session of
	// the user in every tenant. Both are global statements, so the change needs
	// no RLS bypass; the per-tenant session-row revocation is recorded by the
	// follow-up statement after this commits.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		user, updateErr := applyProfileChange(ctx, tx.Stores, id, change)
		if updateErr != nil {
			return updateErr
		}
		updated = user
		if change.emailChanged {
			// The warning goes to the new address and the old one; the job
			// commits with the change.
			if notifyErr := s.Security.Deliver(ctx, tx, SecurityNotice{
				Event:         SecurityEventEmailChanged,
				UserID:        id,
				Name:          user.Name,
				Email:         user.Email,
				PreviousEmail: existing.User.Email,
				OccurredAt:    time.Now().UTC(),
			}); notifyErr != nil {
				return notifyErr
			}
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.profile_updated", id, change.auditMetadata())))
	})
	if err != nil {
		return nil, err
	}
	if change.emailChanged {
		cleanupStaleSessions(ctx, s.Stores.Sessions, id, RevokeCredentialsChanged)
		invalidateSessions(ctx, s.SessionCache, id)
	}
	existing.User = *updated
	return existing, nil
}

// planProfileChange resolves the requested name/email against the stored
// account and checks that a new address is available.
func (s *userService) planProfileChange(ctx context.Context, existing *User, req UpdateProfileInput) (profileChange, error) {
	change := profileChange{
		name:        existing.Name,
		email:       existing.Email,
		updateEmail: req.Email != nil,
	}
	if req.Name != nil {
		change.name = strings.TrimSpace(*req.Name)
		change.nameChanged = change.name != existing.Name
	}
	if req.Email == nil {
		return change, nil
	}
	requested := NormalizeEmail(*req.Email)
	if requested == existing.Email {
		return change, nil
	}
	if err := ensureEmailFreeForChange(ctx, s.Stores, requested); err != nil {
		return profileChange{}, err
	}
	change.email = requested
	change.emailChanged = true
	return change, nil
}

func ensureEmailFreeForChange(ctx context.Context, st Stores, email string) error {
	_, err := st.Users.GetByEmail(ctx, email)
	switch {
	case err == nil:
		return apperror.NewConflict("email already in use")
	case !errors.Is(err, apperror.ErrNotFound):
		return apperror.NewInternal("failed to check email availability")
	}
	return ensureEmailNotBanned(ctx, st.Memberships, email)
}

// applyProfileChange stores the change. Changing the identity address ends
// every session of the user in every tenant through the token-version bump the
// UPDATE makes; the caller records the per-tenant session-row revocation.
func applyProfileChange(ctx context.Context, st Stores, id int64, change profileChange) (*User, error) {
	user, err := st.Users.UpdateProfile(ctx, ProfileUpdate{
		ID:          id,
		Name:        change.name,
		Email:       change.email,
		UpdateEmail: change.updateEmail,
	})
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return nil, apperror.NewConflict("email already in use")
		}
		return nil, userWriteError(err, "user not found", "failed to update user")
	}
	if change.emailChanged {
		if err := st.Resets.DeleteByUser(ctx, id); err != nil {
			return nil, apperror.NewInternal("failed to revoke reset tokens")
		}
	}
	return user, nil
}

// DeleteAccount deletes the caller's global account: every active
// membership is removed (bans stay recorded), every session ends, and the
// address becomes reusable. It refuses when the account is the last active
// admin of any tenant.
func (s *userService) DeleteAccount(ctx context.Context, id int64) error {
	if _, err := getMember(ctx, s.Stores.Memberships, id); err != nil {
		return err
	}
	ctx = database.WithRLSBypass(ctx) // Deleting the global account removes memberships and sessions in every tenant.
	// This is a genuinely cross-tenant transaction: the global user row, the
	// membership in each tenant, and the sessions in each tenant must change
	// together. Every tenant-owned statement below keeps an explicit tenant
	// predicate (tenant.WithID per membership; the audit event's TenantID).
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := deleteAccount(ctx, tx.Stores, id); err != nil {
			return err
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.deleted", id, map[string]any{"scope": "account"})))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.SessionCache, id)
	return nil
}

func deleteAccount(ctx context.Context, st Stores, id int64) error {
	memberships, err := st.Memberships.ListForUser(ctx, id)
	if err != nil {
		return apperror.NewInternal("failed to load memberships")
	}
	// Lock each tenant's admin changes in tenant order (ListForUser sorts by
	// tenant), so two concurrent deletions cannot deadlock on the locks.
	sort.Slice(memberships, func(i, j int) bool { return memberships[i].TenantID < memberships[j].TenantID })
	for i := range memberships {
		m := &memberships[i]
		if m.Status != MembershipStatusActive {
			continue
		}
		tctx := tenant.WithID(ctx, m.TenantID)
		// Take the membership row before the per-tenant owner-change advisory
		// lock: rank checks and bans take row -> advisory, and the reverse
		// order here would deadlock against a concurrent ban/demotion.
		if err := st.Memberships.LockMembership(tctx, id); err != nil {
			return userWriteError(err, "user not found", "failed to lock membership")
		}
		if m.Role == RoleOwner {
			if err := ensureNotLastOwner(tctx, st.Memberships, "cannot delete the account: it is the last active owner of a tenant"); err != nil {
				return err
			}
		}
		if _, err := st.Memberships.Remove(tctx, id); err != nil {
			return userWriteError(err, "user not found", "failed to remove membership")
		}
	}
	if _, err := st.Users.MarkDeleted(ctx, id); err != nil {
		return userWriteError(err, "user not found", "failed to delete user")
	}
	if err := st.Sessions.RevokeAllForUser(ctx, id, RevokeAccountDeleted); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	if err := st.Resets.DeleteByUser(ctx, id); err != nil {
		return apperror.NewInternal("failed to revoke reset tokens")
	}
	if err := st.Verifications.DeleteByUser(ctx, id); err != nil {
		return apperror.NewInternal("failed to revoke verification tokens")
	}
	return nil
}

// RemoveMember removes another user's membership from the request tenant
// (the user's global account and other tenants are untouched). The last
// active admin can never be removed.
func (s *userService) RemoveMember(ctx context.Context, id int64) error {
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := removeMembership(ctx, tx.Stores, id); err != nil {
			return err
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "user.deleted", id, map[string]any{"scope": "membership"})))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.SessionCache, id)
	return nil
}

func removeMembership(ctx context.Context, st Stores, id int64) error {
	const notFound = "user not found"
	if err := EnsureCanManageMember(ctx, st, id); err != nil {
		return err
	}
	member, err := st.Memberships.Get(ctx, id)
	if err != nil {
		return userWriteError(err, notFound, "failed to get user")
	}
	if member.Membership.Status == MembershipStatusRemoved {
		return apperror.NewNotFound(notFound)
	}
	if member.Membership.Role == RoleOwner && member.Membership.Status == MembershipStatusActive {
		if err := ensureNotLastOwner(ctx, st.Memberships, "cannot delete the last active owner"); err != nil {
			return err
		}
	}
	if _, err := st.Memberships.Remove(ctx, id); err != nil {
		return userWriteError(err, notFound, "failed to remove membership")
	}
	if err := st.Sessions.RevokeAllInTenant(ctx, id, RevokeMembershipChanged); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	return nil
}

// RevokeSessions invalidates every access token and session of the user in
// every tenant ("log out all devices").
func (s *userService) RevokeSessions(ctx context.Context, userID int64) error {
	// The token-version bump is a global users statement, so the transaction
	// needs no RLS bypass; it alone ends every token in every tenant. The
	// per-tenant session-row revocation is the follow-up bypass statement.
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if _, err := tx.Users.RevokeTokens(ctx, userID); err != nil {
			return userWriteError(err, "user not found", "failed to revoke sessions")
		}
		return nil
	})
	if err != nil {
		return err
	}
	cleanupStaleSessions(ctx, s.Stores.Sessions, userID, RevokeLogoutAll)
	invalidateSessions(ctx, s.SessionCache, userID)
	return nil
}
