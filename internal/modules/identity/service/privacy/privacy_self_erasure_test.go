package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type recordingPasswordVerifier struct {
	calls int
	err   error
}

func (v *recordingPasswordVerifier) VerifyPassword(context.Context, uuid.UUID, string) error {
	v.calls++
	return v.err
}

func newSelfErasureService(t *testing.T) (*PrivacyService, *fakes.MemDB, *recordingPasswordVerifier) {
	t.Helper()
	stores, db := privacyTestStores(newFakePrivacyRepo())
	verifier := &recordingPasswordVerifier{}
	return NewPrivacyService(PrivacyDeps{Stores: stores, Verifier: verifier}), db, verifier
}

func TestReauthenticateSelfErasure_PasswordAccountNeedsThePassword(t *testing.T) {
	svc, db, verifier := newSelfErasureService(t)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	userID := fakes.TestID(10)
	db.Passwords[userID] = "seed-hash"

	err := svc.reauthenticateSelfErasure(ctx, userID, "", uuid.Nil)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, 400, resolvedStatus(appErr))
	assert.Zero(t, verifier.calls, "an empty password must be rejected without spending an attempt")

	require.NoError(t, svc.reauthenticateSelfErasure(ctx, userID, "correct horse battery staple", uuid.Nil))
	assert.Equal(t, 1, verifier.calls)
}

// An OAuth-only account has no password to offer. It re-authenticates with an
// interactive session no older than core.ReauthSessionMaxAge, the same rule MFA
// and passkey changes use.
func TestReauthenticateSelfErasure_PasswordlessAccountNeedsARecentSession(t *testing.T) {
	svc, db, verifier := newSelfErasureService(t)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	userID := fakes.TestID(10)
	sessionID := fakes.TestID(700)
	now := time.Now()
	db.Sessions[sessionID] = &core.Session{
		ID: sessionID, TenantID: tenant.DefaultID, UserID: userID,
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}

	require.NoError(t, svc.reauthenticateSelfErasure(ctx, userID, "", sessionID))
	assert.Zero(t, verifier.calls, "a passwordless account never verifies a password")

	db.Sessions[sessionID].CreatedAt = now.Add(-core.ReauthSessionMaxAge - time.Minute)
	err := svc.reauthenticateSelfErasure(ctx, userID, "", sessionID)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeAuthMFAReauthRequired), appErr.Code)

	revokedAt := now.Add(-time.Second)
	db.Sessions[sessionID].CreatedAt = now
	db.Sessions[sessionID].RevokedAt = &revokedAt
	err = svc.reauthenticateSelfErasure(ctx, userID, "", sessionID)
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeAuthMFAReauthRequired), appErr.Code)
}

func TestReauthenticateSelfErasure_ForeignSessionIsRefused(t *testing.T) {
	svc, db, _ := newSelfErasureService(t)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	userID := fakes.TestID(10)
	sessionID := fakes.TestID(701)
	now := time.Now()
	db.Sessions[sessionID] = &core.Session{
		ID: sessionID, TenantID: tenant.DefaultID, UserID: fakes.TestID(11),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}

	err := svc.reauthenticateSelfErasure(ctx, userID, "", sessionID)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeAuthMFAReauthRequired), appErr.Code)
}
