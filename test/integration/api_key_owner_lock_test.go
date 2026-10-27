//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	apikeyadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/adapter"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/banhash"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type ownerLockHarness struct {
	pool     *pgxpool.Pool
	keys     apikeyservice.Service
	admin    identityservice.AdminUserService
	verifier *apikey.Verifier
	owner    uuid.UUID
}

// setupOwnerLockHarness wires the production identity adapter and real
// transactions, which the owner-lock fix depends on.
func setupOwnerLockHarness(t *testing.T) *ownerLockHarness {
	t.Helper()
	ctx := context.Background()
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	insertAdmin := func(email string) uuid.UUID {
		return testutil.CreateMember(t, ctx, pool, tenant.DefaultID, email, testutil.MemberOptions{Role: "admin"}).UserID
	}
	owner := insertAdmin("lock-owner@test.com")
	insertAdmin("lock-other-admin@test.com") // the owner is not the last owner

	txManager := fx.Tx()
	stores := identityrepo.NewStores(fx.Scoped(), testutil.BanHasher(t))
	principals := identityadapter.NewPrincipalReader(stores.Memberships)
	keyRepo := apikeyrepo.NewAPIKeyRepository(pool)
	return &ownerLockHarness{
		pool:     pool,
		keys:     apikeyservice.NewService(keyRepo, principals, nil, apikeyrepo.NewTransactor(txManager, audit.Noop{}, principals), authz.NewAuthorizer(integrationCatalogs().Permissions), 8760*time.Hour),
		admin:    identityservice.NewAdminUserService(stores, identityrepo.NewTransactor(txManager, audit.Noop{}, nil, nil, testutil.BanHasher(t)), nil),
		verifier: apikey.NewVerifier(apikeyadapter.NewVerifierStore(keyRepo), principals),
		owner:    owner,
	}
}

func ownerLockCtx() context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
}

func (h *ownerLockHarness) createKey(ctx context.Context) (*apikeyservice.CreatedKey, error) {
	return h.keys.Create(ctx, h.owner, apikeyservice.CreateInput{Name: "lock", Scopes: []string{"files:read"}})
}

// A key created while its owner's ban is in flight must not be minted: the
// create waits for the ban and then sees no active owner.
func TestAPIKeyCreateWaitsForInFlightBan(t *testing.T) {
	h := setupOwnerLockHarness(t)
	ctx := ownerLockCtx()

	ban, err := h.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = ban.Exec(ctx, `UPDATE memberships SET status = 'suspended', banned_at = NOW(),
		credentials_revoked_at = clock_timestamp() WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, h.owner)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, createErr := h.createKey(ctx)
		done <- createErr
	}()
	select {
	case createErr := <-done:
		t.Fatalf("create must wait for the in-flight ban, returned %v", createErr)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, ban.Commit(ctx))

	createErr := <-done
	var appErr *apperror.AppError
	require.ErrorAs(t, createErr, &appErr)
	require.Equal(t, http.StatusNotFound, resolvedStatus(appErr))

	var keys int64
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id = $1`, h.owner).Scan(&keys))
	require.Zero(t, keys)
}

// A ban whose transaction started before a key create, but whose UPDATE
// waits for that create's owner lock, must still revoke the key: the key's
// created_at (its transaction start) is later than the ban transaction's
// start, so credentials_revoked_at must be taken when the UPDATE runs.
func TestAPIKeyCreatedDuringBanStaysRevokedAfterUnban(t *testing.T) {
	h := setupOwnerLockHarness(t)
	ctx := ownerLockCtx()

	banTx, err := h.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = banTx.Rollback(ctx) }()
	_, err = banTx.Exec(ctx, `SELECT now()`) // pin the ban transaction's start time
	require.NoError(t, err)
	time.Sleep(20 * time.Millisecond)

	// Hold the owner row the way Create does and mint the key.
	keyTx, err := h.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = keyTx.Rollback(ctx) }()
	_, err = identityadapter.NewPrincipalReader(identityrepo.NewMembershipRepository(h.pool)).
		LockPrincipalTx(ctx, keyTx, tenant.DefaultID, h.owner)
	require.NoError(t, err)
	credentials, err := apikey.Generate()
	require.NoError(t, err)
	_, err = apikeyrepo.NewAPIKeyRepository(keyTx).Create(ctx, apikeyservice.NewKey{
		UserID: h.owner, Name: "racing", Prefix: credentials.Prefix, Hash: credentials.Hash,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	banHasher, err := banhash.New(strings.Join([]string{"owner", "lock", "ban", "pepper"}, "-"))
	require.NoError(t, err)
	banHash, banKid := banHasher.Marker("owner-lock-ban@example.com")

	banned := make(chan error, 1)
	go func() {
		_, banErr := identityrepo.NewMembershipRepository(banTx).Ban(ctx, h.owner, identityservice.BanMarker{Hash: banHash, KeyID: banKid})
		banned <- banErr
	}()
	time.Sleep(300 * time.Millisecond) // the ban UPDATE is now blocked on the owner row
	require.NoError(t, keyTx.Commit(ctx))
	require.NoError(t, <-banned)
	require.NoError(t, banTx.Commit(ctx))

	_, err = h.admin.UnbanUser(ctx, h.owner)
	require.NoError(t, err)
	_, err = h.verifier.Verify(ctx, credentials.Key, tenant.DefaultID)
	require.Error(t, err, "a key created before the ban committed must stay revoked after unban")
}

// Rotating a key that a ban revoked must not mint a live successor after
// unban.
func TestAPIKeyRotateRefusesKeyRevokedByBan(t *testing.T) {
	h := setupOwnerLockHarness(t)
	ctx := ownerLockCtx()

	created, err := h.createKey(ctx)
	require.NoError(t, err)
	require.NoError(t, h.admin.BanUser(ctx, h.owner))
	_, err = h.admin.UnbanUser(ctx, h.owner)
	require.NoError(t, err)

	_, err = h.keys.Rotate(ctx, h.owner, created.ID)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, http.StatusConflict, resolvedStatus(appErr))

	// A key created after the unban is unaffected.
	fresh, err := h.createKey(ctx)
	require.NoError(t, err)
	_, err = h.keys.Rotate(ctx, h.owner, fresh.ID)
	require.NoError(t, err)
}
