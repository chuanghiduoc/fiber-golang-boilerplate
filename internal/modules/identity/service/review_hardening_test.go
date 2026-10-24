package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

func appErrorOf(t *testing.T, err error) *apperror.AppError {
	t.Helper()
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	return appErr
}

func ipCtx(ip string) context.Context {
	return WithClientIP(context.Background(), ip)
}

// ---------------------------------------------------------------------------
// Login lockout keyed by (tenant, email, client IP) plus a global budget
// ---------------------------------------------------------------------------

func registerLockoutUser(t *testing.T, svc UserService) {
	t.Helper()
	_, err := svc.Register(context.Background(), dto.RegisterRequest{
		Email: "victim@example.com", Password: "Password1!", Name: "Victim",
	})
	require.NoError(t, err)
}

func TestLockoutFromOneSourceDoesNotLockOtherSources(t *testing.T) {
	svc := NewUserService(newMockUserRepo(), newMockRefreshTokenRepo(), false, cache.NewMemoryCache(), nil, audit.Noop{})
	registerLockoutUser(t, svc)

	// The attacker keeps hammering long after its own lockout; locked
	// attempts must not drain the global per-account budget.
	for range maxAccountLoginAttempts * 2 {
		_, _ = svc.Authenticate(ipCtx("203.0.113.9"), dto.LoginRequest{Email: "victim@example.com", Password: "Wrong!"})
	}
	_, err := svc.Authenticate(ipCtx("203.0.113.9"), dto.LoginRequest{Email: "victim@example.com", Password: "Password1!"})
	assert.Equal(t, http.StatusTooManyRequests, loginStatus(err), "the attacking source is locked")

	user, err := svc.Authenticate(ipCtx("198.51.100.7"), dto.LoginRequest{Email: "victim@example.com", Password: "Password1!"})
	require.NoError(t, err, "the victim's own source must still sign in")
	assert.Equal(t, "victim@example.com", user.Email)
}

func TestLockoutGlobalBudgetStopsDistributedGuessing(t *testing.T) {
	svc := NewUserService(newMockUserRepo(), newMockRefreshTokenRepo(), false, cache.NewMemoryCache(), nil, audit.Noop{})
	registerLockoutUser(t, svc)

	sources := maxAccountLoginAttempts / maxLoginAttempts
	for i := range sources {
		ctx := ipCtx("192.0.2." + string(rune('a'+i)))
		for range maxLoginAttempts {
			_, err := svc.Authenticate(ctx, dto.LoginRequest{Email: "victim@example.com", Password: "Wrong!"})
			require.Equal(t, http.StatusUnauthorized, loginStatus(err), "within both budgets")
		}
	}
	_, err := svc.Authenticate(ipCtx("192.0.2.250"), dto.LoginRequest{Email: "victim@example.com", Password: "Password1!"})
	assert.Equal(t, http.StatusTooManyRequests, loginStatus(err), "a fresh source is locked once the account budget is spent")
}

func TestVerifyPasswordUsesPerSourceLockout(t *testing.T) {
	repo := newMockUserRepo()
	svc := NewUserService(repo, newMockRefreshTokenRepo(), false, cache.NewMemoryCache(), nil, audit.Noop{})
	registerLockoutUser(t, svc)

	for range maxLoginAttempts + 1 {
		_, _ = svc.Authenticate(ipCtx("203.0.113.9"), dto.LoginRequest{Email: "victim@example.com", Password: "Wrong!"})
	}
	assert.Equal(t, http.StatusTooManyRequests, loginStatus(svc.VerifyPassword(ipCtx("203.0.113.9"), 1, "Password1!")),
		"re-authentication shares the source counter")
	assert.NoError(t, svc.VerifyPassword(ipCtx("198.51.100.7"), 1, "Password1!"),
		"another source's lockout must not block re-authentication")
}

type failingIncrementCache struct{ cache.Cache }

func (failingIncrementCache) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("redis: connection refused")
}

func TestLockoutCacheFailureIsServiceUnavailable(t *testing.T) {
	repo := newMockUserRepo()
	svc := NewUserService(repo, newMockRefreshTokenRepo(), false, failingIncrementCache{cache.NewMemoryCache()}, nil, audit.Noop{})

	_, err := svc.Authenticate(context.Background(), dto.LoginRequest{Email: "victim@example.com", Password: "Wrong!"})
	appErr := appErrorOf(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, appErr.Status)
	assert.Equal(t, apperror.CodeSystemUnavailable, apperror.Code(appErr.Code))
}

// ---------------------------------------------------------------------------
// Google sign-in: link guard and Tier 1 audit
// ---------------------------------------------------------------------------

func TestFindOrCreateByGoogleNeverReplacesAnotherGoogleIdentity(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	repo.users[1] = &identitysqlc.User{
		TenantID: 1, ID: 1, Email: "linked@example.com", Name: "Linked", Role: "user",
		GoogleID:        pgtype.Text{String: "google-original", Valid: true},
		AuthProvider:    "google",
		EmailVerifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	repo.nextID = 2

	_, err := svc.SignInWithGoogle(context.Background(), GoogleSignIn{GoogleID: "google-attacker", Email: "linked@example.com", Name: "Attacker", AllowSignUp: true})
	assert.Equal(t, http.StatusConflict, appErrorOf(t, err).Status)
	assert.Equal(t, "google-original", repo.users[1].GoogleID.String)
}

type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, event audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recordingAudit) RecordTx(ctx context.Context, _ pgx.Tx, event audit.Event) error {
	return r.Record(ctx, event)
}

func TestFindOrCreateByGoogleAuditsStateTenantAndUser(t *testing.T) {
	repo := newMockUserRepo()
	recorder := &recordingAudit{}
	svc := NewUserService(repo, newMockRefreshTokenRepo(), false, newMockCache(), nil, recorder)

	// The callback skips tenant middleware: request-level audit info has no
	// tenant or actor, and the handler installs the signed-state tenant.
	ctx := audit.WithRequestInfo(context.Background(), &audit.RequestInfo{RequestID: "req-1"})
	ctx = tenant.WithID(ctx, 2)
	user, err := svc.SignInWithGoogle(ctx, GoogleSignIn{GoogleID: "google-2", Email: "oauth@example.com", Name: "OAuth", AllowSignUp: true})
	require.NoError(t, err)

	require.Len(t, recorder.events, 1)
	event := recorder.events[0]
	assert.Equal(t, "auth.google_login", event.Action)
	assert.Equal(t, int64(2), event.TenantID, "state tenant, not the default tenant")
	assert.Equal(t, user.ID, event.ActorID)
	assert.Equal(t, audit.ActorTypeUser, event.ActorType)
	assert.Equal(t, googleAccountCreated, event.Metadata["account"])
}

// A sign-in the caller refuses (here: unverified email) must not be audited
// as a successful auth.google_login.
func TestSignInWithGoogleRefusedByCallerIsNotAudited(t *testing.T) {
	repo := newMockUserRepo()
	repo.users[1] = &identitysqlc.User{
		TenantID: 1, ID: 1, Email: "changed@example.com", Name: "Changed", Role: "user",
		GoogleID: pgtype.Text{String: "google-1", Valid: true}, AuthProvider: "google",
	}
	repo.nextID = 2
	recorder := &recordingAudit{}
	svc := NewUserService(repo, newMockRefreshTokenRepo(), false, newMockCache(), nil, recorder)

	refused := apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	_, err := svc.SignInWithGoogle(context.Background(), GoogleSignIn{
		GoogleID: "google-1", Email: "changed@example.com", Name: "Changed", AllowSignUp: true,
		Complete: func(context.Context, pgx.Tx, *dto.UserResponse) error { return refused },
	})
	require.ErrorIs(t, err, refused)
	assert.Empty(t, recorder.events, "a refused sign-in must not write auth.google_login")
}

// ---------------------------------------------------------------------------
// Forgot password: constant-shape HTTP path, work in a durable job
// ---------------------------------------------------------------------------

type recordingQueue struct {
	mu   sync.Mutex
	jobs []jobs.Job
	err  error
}

func (q *recordingQueue) Enqueue(_ context.Context, job jobs.Job) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return 0, q.err
	}
	q.jobs = append(q.jobs, job)
	return int64(len(q.jobs)), nil
}

// failingLookupRepo makes any account read fail. The HTTP path must never
// read the account, otherwise only registered addresses could see an error.
type failingLookupRepo struct{ *mockUserRepo }

func (failingLookupRepo) GetByEmail(context.Context, string) (*identitysqlc.User, error) {
	return nil, errors.New("database is down")
}

func TestForgotPasswordIsConstantShapeWithQueue(t *testing.T) {
	codec, err := jobs.NewPayloadCodec("forgot-password-test-secret-0123456789")
	require.NoError(t, err)
	resetRepo := newMockPasswordResetRepo()
	queue := &recordingQueue{}
	svc := NewPasswordResetService(failingLookupRepo{newMockUserRepo()}, resetRepo, newMockRefreshTokenRepo(),
		newMockEmailSender(), newMockCache(), "http://localhost:3000", nil)
	AttachResetRequestQueue(svc, queue, codec)

	ctx := tenant.WithIdentity(context.Background(), 2, "acme")
	for _, addr := range []string{"known@example.com", "unknown@example.com"} {
		require.NoError(t, svc.ForgotPassword(ctx, dto.ForgotPasswordRequest{Email: addr}),
			"every address gets the same response without touching the account")
	}

	require.Len(t, queue.jobs, 2)
	for _, job := range queue.jobs {
		assert.Equal(t, ResetRequestJobKind, job.Kind)
		assert.NotContains(t, string(job.Payload), "@example.com", "the address is sealed at rest")
	}
	assert.Empty(t, resetRepo.tokens, "no token work on the request path")

	// An enqueue failure is the only error, and it is address-independent.
	queue.err = errors.New("jobs table unavailable")
	assert.Equal(t, http.StatusInternalServerError,
		appErrorOf(t, svc.ForgotPassword(ctx, dto.ForgotPasswordRequest{Email: "other@example.com"})).Status)
}

func TestHandleResetRequestJobCreatesTokenAndEmailIdempotently(t *testing.T) {
	codec, err := jobs.NewPayloadCodec("forgot-password-test-secret-0123456789")
	require.NoError(t, err)
	userRepo := newMockUserRepo()
	hash, err := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	require.NoError(t, err)
	userRepo.users[1] = &identitysqlc.User{TenantID: 2, ID: 1, Email: "known@example.com", Name: "Known", Role: "user",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true}}
	resetRepo := newMockPasswordResetRepo()
	sender := newMockEmailSender()
	queue := &recordingQueue{}
	svc := NewPasswordResetService(userRepo, resetRepo, newMockRefreshTokenRepo(), sender, newMockCache(), "http://localhost:3000", nil)
	AttachResetRequestQueue(svc, queue, codec)

	ctx := tenant.WithIdentity(context.Background(), 2, "acme")
	require.NoError(t, svc.ForgotPassword(ctx, dto.ForgotPasswordRequest{Email: "Known@Example.com"}))
	require.NoError(t, svc.ForgotPassword(tenant.WithID(context.Background(), 2), dto.ForgotPasswordRequest{Email: "nobody@example.com"}))
	require.Len(t, queue.jobs, 2)

	// The worker restores only the job's tenant ID.
	workerCtx := tenant.WithID(context.Background(), 2)
	for range 2 { // at-least-once: a duplicate delivery replaces the token
		require.NoError(t, svc.HandleResetRequestJob(workerCtx, queue.jobs[0].Payload))
	}
	require.NoError(t, svc.HandleResetRequestJob(workerCtx, queue.jobs[1].Payload), "unknown address is a silent no-op")

	assert.Len(t, resetRepo.tokens, 1, "one live token per user")
	require.True(t, sender.WaitFor(2, 2*time.Second))
	last := sender.Messages()[len(sender.Messages())-1]
	assert.Equal(t, []string{"known@example.com"}, last.To)
	assert.True(t, strings.Contains(last.HTML, "tenant=acme"), "the link keeps the request tenant slug")

	var malformed json.RawMessage = []byte(`{"email":""}`)
	assert.Error(t, svc.HandleResetRequestJob(workerCtx, malformed), "a job without an address fails loudly")
}
