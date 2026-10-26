//go:build integration

package service_test

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

func init() {
	if err := identitycatalog.Register(); err != nil {
		panic(err)
	}
}

type mailbox struct {
	mu   sync.Mutex
	msgs []email.Message
}

func (m *mailbox) Send(_ context.Context, msg email.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

var linkToken = regexp.MustCompile(`token=([0-9a-f]{64})`)

func (m *mailbox) tokenFor(t *testing.T, addr string) string {
	t.Helper()
	var tok string
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i := len(m.msgs) - 1; i >= 0; i-- {
			if m.msgs[i].To[0] == addr {
				if match := linkToken.FindStringSubmatch(m.msgs[i].HTML); match != nil {
					tok = match[1]
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
	return tok
}

type env struct {
	pool     *pgxpool.Pool
	stores   service.Stores
	tx       *identityrepo.Transactor
	users    service.UserService
	admin    service.AdminUserService
	sessions *service.SessionService
	mfa      service.MFAService
	resets   service.PasswordResetService
	verify   service.EmailVerificationService
	mail     *mailbox
}

func newEnv(t *testing.T, recorder audit.TxRecorder) *env {
	t.Helper()
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })

	if recorder == nil {
		recorder = audit.NewPostgresRecorder(testutil.Scoped(pool))
	}
	stores := identityrepo.NewStores(testutil.Scoped(pool))
	tx := identityrepo.NewTransactor(testutil.NewTxManager(pool), recorder, nil, nil)
	mail := &mailbox{}
	sign := func(c service.AccessClaims) (string, error) { return "access", nil }
	sessions := service.NewSessionService(stores, tx, sign, time.Hour, 720*time.Hour, nil)
	verify := service.NewEmailVerificationService(service.VerificationDeps{
		Stores: stores, Tx: tx, Sender: mail, Cache: appCache, FrontendURL: "https://app.example.test",
	})
	return &env{
		pool:   pool,
		stores: stores,
		tx:     tx,
		users: service.NewUserService(service.UserDeps{
			Stores: stores, Tx: tx, Cache: appCache, Sessions: sessions, Verification: verify,
		}),
		admin:    service.NewAdminUserService(stores, tx, nil),
		sessions: sessions,
		mfa: service.NewMFAService(service.MFADeps{
			Stores: stores, Tx: tx, Cache: appCache, Sealer: testSealer{}, Sessions: sessions,
		}),
		resets: service.NewPasswordResetService(service.ResetDeps{
			Stores: stores, Tx: tx, Sender: mail, Cache: appCache, FrontendURL: "https://app.example.test",
		}),
		verify: verify,
		mail:   mail,
	}
}

func appCode(err error) string {
	if appErr, ok := err.(*apperror.AppError); ok {
		return appErr.Code
	}
	return ""
}

func appStatus(err error) int {
	if appErr, ok := err.(*apperror.AppError); ok {
		return appErr.Status
	}
	return 0
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}

// Registration is Tier 1: when the audit row cannot be written the user,
// password, and membership roll back together.
func TestRegister_RollsBackEverythingWhenAuditFails(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	broken := identityrepo.NewTransactor(testutil.NewTxManager(e.pool), nil, nil, nil) // nil recorder fails closed
	users := service.NewUserService(service.UserDeps{Stores: e.stores, Tx: broken, Cache: cache.NewMemoryCache()})

	_, err := users.Register(ctx, service.RegisterInput{Email: "atomic@example.com", Password: "Password1!", Name: "Atomic"})
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), appCode(err))
	assert.Zero(t, scalar[int64](t, e.pool, `SELECT count(*) FROM users`))
	assert.Zero(t, scalar[int64](t, e.pool, `SELECT count(*) FROM memberships`))
	assert.Zero(t, scalar[int64](t, e.pool, `SELECT count(*) FROM user_passwords`))
}

// The last active owner of a tenant can be neither demoted, banned, removed,
// nor deleted with their account; the trigger on memberships backs the
// service checks.
func TestLastOwnerProtection(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "acme")
	owner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "owner@example.com", testutil.MemberOptions{Role: "owner"})
	// The same user is a plain member elsewhere, which must not count.
	testutil.CreateMember(t, ctx, e.pool, acmeID, "owner@example.com", testutil.MemberOptions{Role: "member"})
	testutil.CreateMember(t, ctx, e.pool, acmeID, "second@example.com", testutil.MemberOptions{Role: "owner"})

	_, err := e.admin.UpdateRole(ctx, owner.UserID, service.RoleMember)
	assert.Equal(t, string(apperror.CodeUserLastAdmin), appCode(err))
	assert.Equal(t, string(apperror.CodeUserLastAdmin), appCode(e.admin.BanUser(ctx, owner.UserID)))
	assert.Equal(t, string(apperror.CodeUserLastAdmin), appCode(e.users.DeleteAccount(ctx, owner.UserID)),
		"deleting the account would leave the default tenant without an owner")

	// Removing them from acme is fine: acme has another owner.
	require.NoError(t, e.users.RemoveMember(tenant.WithID(ctx, acmeID), owner.UserID))
	assert.Equal(t, "removed", scalar[string](t, e.pool,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, acmeID, owner.UserID))

	// The database trigger refuses a direct write too.
	_, err = e.pool.Exec(ctx, `DELETE FROM membership_roles WHERE tenant_id = 1 AND user_id = $1`, owner.UserID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last owner")
}

// A ban is per membership and survives the banned account deleting itself:
// the address cannot register into (or be invited to) that tenant again.
func TestBanSurvivesAccountDeletion(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "admin@example.com", testutil.MemberOptions{Role: "admin"})
	user, err := e.users.Register(ctx, service.RegisterInput{Email: "banned@example.com", Password: "Password1!", Name: "Banned"})
	require.NoError(t, err)
	require.NoError(t, e.admin.BanUser(ctx, user.User.ID))
	assert.NotNil(t, scalar[*time.Time](t, e.pool,
		`SELECT credentials_revoked_at FROM memberships WHERE tenant_id = 1 AND user_id = $1`, user.User.ID),
		"a ban revokes the member's API keys")

	// The banned user can still delete their global account (from another
	// tenant, where they are a member).
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "acme")
	testutil.CreateMember(t, ctx, e.pool, acmeID, "banned@example.com", testutil.MemberOptions{})
	require.NoError(t, e.users.DeleteAccount(tenant.WithID(ctx, acmeID), user.User.ID))
	assert.Equal(t, "suspended", scalar[string](t, e.pool,
		`SELECT status FROM memberships WHERE tenant_id = 1 AND user_id = $1`, user.User.ID), "the ban stays recorded")

	_, err = e.users.Register(ctx, service.RegisterInput{Email: "banned@example.com", Password: "Password1!", Name: "Again"})
	assert.Equal(t, http.StatusConflict, appStatus(err))
	again, err := e.users.Register(tenant.WithID(ctx, acmeID), service.RegisterInput{Email: "banned@example.com", Password: "Password1!", Name: "Again"})
	require.NoError(t, err, "the address is free elsewhere once the account is deleted")
	assert.NotEqual(t, user.User.ID, again.User.ID)
}

// Two concurrent rotations of one refresh token: exactly one succeeds and the
// loser is treated as reuse, which revokes the family.
func TestRefresh_ConcurrentRotationRevokesFamily(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	user, err := e.users.Register(ctx, service.RegisterInput{Email: "race@example.com", Password: "Password1!", Name: "Race"})
	require.NoError(t, err)
	pair, err := e.sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = e.sessions.Refresh(ctx, pair.RefreshToken, nil)
		}()
	}
	wg.Wait()
	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), appCode(err))
		}
	}
	assert.LessOrEqual(t, succeeded, 1, "a refresh token rotates at most once")
	assert.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM refresh_tokens WHERE session_id = $1 AND used_at IS NOT NULL`, pair.SessionID))
	if succeeded == 1 {
		assert.Equal(t, "refresh_reuse", scalar[string](t, e.pool,
			`SELECT revoked_reason FROM sessions WHERE id = $1`, pair.SessionID))
	}
}

// Password reset is per user: the token works without a tenant, revokes every
// session in every tenant, and later account changes invalidate older links.
func TestPasswordReset_IsGlobal(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	acmeID := testutil.CreateTenant(t, ctx, e.pool, "acme")
	user, err := e.users.Register(ctx, service.RegisterInput{Email: "reset@example.com", Password: "Password1!", Name: "Reset"})
	require.NoError(t, err)
	testutil.CreateMember(t, ctx, e.pool, acmeID, "reset@example.com", testutil.MemberOptions{})
	inDefault, err := e.sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)
	inAcme, err := e.sessions.Issue(tenant.WithID(ctx, acmeID), user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	require.NoError(t, e.resets.ForgotPassword(tenant.WithIdentity(ctx, acmeID, "acme"), service.ForgotPasswordInput{Email: "Reset@Example.com"}))
	resetToken := e.mail.tokenFor(t, "reset@example.com")

	// Reset from a request in the default tenant: the link is not tenant-bound.
	require.NoError(t, e.resets.ResetPassword(ctx, service.ResetPasswordInput{Token: resetToken, Password: "NewPassword2@"}))
	for _, sid := range []int64{inDefault.SessionID, inAcme.SessionID} {
		assert.Equal(t, "credentials_changed", scalar[string](t, e.pool, `SELECT revoked_reason FROM sessions WHERE id = $1`, sid))
	}
	_, err = e.users.Authenticate(tenant.WithID(ctx, acmeID), service.LoginInput{Email: "reset@example.com", Password: "NewPassword2@"})
	assert.NoError(t, err, "the new password works in every tenant")
	assert.Equal(t, http.StatusBadRequest, appStatus(e.resets.ResetPassword(ctx, service.ResetPasswordInput{Token: resetToken, Password: "Another3#"})),
		"single use")
}

func TestEmailVerification_IsPerUser(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	user, err := e.users.Register(ctx, service.RegisterInput{Email: "verify@example.com", Password: "Password1!", Name: "V"})
	require.NoError(t, err)
	require.NoError(t, e.verify.SendVerification(tenant.WithIdentity(ctx, tenant.DefaultID, "default"), user.User.ID, user.User.Email))
	tok := e.mail.tokenFor(t, "verify@example.com")

	acmeID := testutil.CreateTenant(t, ctx, e.pool, "acme")
	require.NoError(t, e.verify.Verify(tenant.WithID(ctx, acmeID), tok), "verification is not tenant-bound")
	assert.NotNil(t, scalar[*time.Time](t, e.pool, `SELECT email_verified_at FROM users WHERE id = $1`, user.User.ID))
	assert.Equal(t, http.StatusBadRequest, appStatus(e.verify.Verify(ctx, tok)), "single use")
}

func TestTenantService_CreateAndLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	tenants := service.NewTenantService(e.stores, e.tx)
	trialEnd := time.Now().Add(14 * 24 * time.Hour)

	created, err := tenants.Create(ctx, service.NewTenant{Slug: "trial-co", Name: "Trial Co", Status: service.TenantStatusTrialing, TrialEndsAt: &trialEnd, Timezone: "Asia/Ho_Chi_Minh"})
	require.NoError(t, err)
	settings, err := tenants.Settings(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "Asia/Ho_Chi_Minh", settings.Timezone)
	assert.Equal(t, "en", settings.Locale)
	_, err = tenants.Create(ctx, service.NewTenant{Slug: "trial-co", Name: "Dup", Status: service.TenantStatusActive})
	assert.Equal(t, http.StatusConflict, appStatus(err))

	// Trialing tenants serve requests; suspended ones do not.
	_, err = e.stores.Tenants.GetOperationalBySlug(ctx, "trial-co")
	require.NoError(t, err)
	_, err = tenants.Transition(ctx, created.ID, service.TenantStatusSuspended)
	require.NoError(t, err)
	_, err = e.stores.Tenants.GetOperationalBySlug(ctx, "trial-co")
	assert.ErrorIs(t, err, apperror.ErrNotFound)

	_, err = tenants.Transition(ctx, created.ID, service.TenantStatusPendingDeletion)
	require.NoError(t, err)
	deleted, err := tenants.Transition(ctx, created.ID, service.TenantStatusDeleted)
	require.NoError(t, err)
	assert.NotNil(t, deleted.DeletionScheduledAt, "the schedule is kept as history")
	_, err = tenants.Transition(ctx, created.ID, service.TenantStatusActive)
	assert.Equal(t, http.StatusConflict, appStatus(err), "deleted is terminal")
	assert.Equal(t, int64(4), scalar[int64](t, e.pool,
		`SELECT count(*) FROM audit_events WHERE resource = 'tenant' AND resource_id = $1`, strconv.FormatInt(created.ID, 10)))

	// The schema backs the state machine's invariants.
	_, err = e.pool.Exec(ctx, `UPDATE tenants SET status = 'deleted', deletion_scheduled_at = NOW() WHERE id = 1`)
	assert.Error(t, err, "the default tenant can never be deleted")
	_, err = e.pool.Exec(ctx, `UPDATE tenants SET status = 'trialing', trial_ends_at = NULL WHERE id = 1`)
	assert.Error(t, err, "a trialing tenant needs a trial end")

	baseline, configured, err := service.StartupTenants(ctx, e.stores.Tenants, "trial-co")
	require.NoError(t, err, "a deleted configured tenant still exists for startup checks")
	assert.Equal(t, int64(1), baseline.ID)
	assert.Equal(t, created.ID, configured.ID)
	_, _, err = service.StartupTenants(ctx, e.stores.Tenants, "missing")
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}
