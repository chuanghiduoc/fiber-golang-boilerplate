package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

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

func (s *userService) Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error) {
	existing, err := s.getUser(ctx, id)
	if err != nil {
		return nil, err
	}
	change, err := s.planProfileChange(ctx, existing, req)
	if err != nil {
		return nil, err
	}

	var user *identitysqlc.User
	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		updated, updateErr := applyProfileChange(ctx, r, id, change)
		if updateErr != nil {
			return updateErr
		}
		user = updated
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, userEvent(ctx, "user.profile_updated", id, change.auditMetadata())))
	})
	if err != nil {
		return nil, err
	}
	if change.emailChanged {
		invalidateSessions(ctx, s.sessions, id)
	}
	return ToUserResponse(user), nil
}

// planProfileChange resolves the requested name/email against the stored
// account and checks that a new address is available.
func (s *userService) planProfileChange(ctx context.Context, existing *identitysqlc.User, req dto.UpdateUserRequest) (profileChange, error) {
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
	if requested == NormalizeEmail(existing.Email) {
		return change, nil
	}
	if err := s.ensureEmailFreeForChange(ctx, requested); err != nil {
		return profileChange{}, err
	}
	change.email = requested
	change.emailChanged = true
	return change, nil
}

func (s *userService) ensureEmailFreeForChange(ctx context.Context, email string) error {
	_, err := s.repo.GetByEmail(ctx, email)
	switch {
	case err == nil:
		return apperror.NewConflict("email already in use")
	case !errors.Is(err, apperror.ErrNotFound):
		return apperror.NewInternal("failed to check email availability")
	}
	return ensureEmailNotBanned(ctx, s.repo, email)
}

// applyProfileChange stores the change. Changing the identity address ends
// every existing session (the UPDATE also bumps the token version).
func applyProfileChange(ctx context.Context, r repos, id int64, change profileChange) (*identitysqlc.User, error) {
	user, err := r.users.Update(ctx, identitysqlc.UpdateUserParams{
		ID:          id,
		Name:        change.name,
		Email:       change.email,
		UpdateEmail: change.updateEmail,
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("email already in use")
		}
		return nil, apperror.NewInternal("failed to update user")
	}
	if change.emailChanged {
		if err := r.refresh.DeleteByUserID(ctx, id); err != nil {
			return nil, apperror.NewInternal("failed to revoke sessions")
		}
	}
	return user, nil
}

func (s *userService) Delete(ctx context.Context, id int64) error {
	target, err := s.getUser(ctx, id)
	if err != nil {
		return err
	}
	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		if deleteErr := deleteAccount(ctx, r, target); deleteErr != nil {
			return deleteErr
		}
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, userEvent(ctx, "user.deleted", id, nil)))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessions, id)
	return nil
}

// deleteAccount soft-deletes target and revokes its refresh tokens, so a
// deleted user's sessions cannot outlive the account.
func deleteAccount(ctx context.Context, r repos, target *identitysqlc.User) error {
	// Checked on the transaction's repository, under the per-tenant
	// admin-change lock, so it serializes with concurrent demotions/bans.
	if target.Role == dto.RoleAdmin {
		if err := ensureNotLastAdmin(ctx, r.users, "cannot delete the last active admin"); err != nil {
			return err
		}
	}
	if _, err := r.users.Delete(ctx, target.ID); err != nil {
		return userWriteError(err, "user not found", "failed to delete user")
	}
	if err := r.refresh.DeleteByUserID(ctx, target.ID); err != nil {
		return apperror.NewInternal("failed to revoke refresh tokens")
	}
	return nil
}

// RevokeSessions invalidates every access and refresh token for a user, used
// by the "logout all devices" endpoint.
func (s *userService) RevokeSessions(ctx context.Context, userID int64) error {
	err := inTx(ctx, s.txManager, s.fallbackRepos(), func(_ pgx.Tx, r repos) error {
		if _, err := r.users.RevokeSessions(ctx, userID); err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to revoke sessions")
		}
		if err := r.refresh.DeleteByUserID(ctx, userID); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessions, userID)
	return nil
}
