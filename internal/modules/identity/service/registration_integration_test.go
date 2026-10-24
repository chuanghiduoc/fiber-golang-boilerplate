//go:build integration

package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// newRegistrationServices wires the real repositories, queue, and services so
// registration exercises the same transactional path as production.
func newRegistrationServices(t *testing.T, pool *pgxpool.Pool) UserService {
	t.Helper()

	appCache := cache.NewMemoryCache()
	txManager := database.NewTxManager(pool)
	userRepo := repository.NewUserRepository(pool)
	verifSvc := NewEmailVerificationService(
		userRepo,
		repository.NewEmailVerificationRepository(pool),
		jobs.NewQueuedSender(jobs.NewQueue(pool)),
		appCache,
		"http://localhost:3000",
		txManager,
	)
	userSvc := NewUserService(
		userRepo,
		repository.NewRefreshTokenRepository(pool),
		true,
		appCache,
		txManager,
		audit.NewPostgresRecorder(pool),
	)
	userSvc.SetVerificationProvisioner(verifSvc)
	require.True(t, userSvc.RegistrationEmailsTransactional(),
		"the provisioner must make registration emails transactional")
	return userSvc
}

// TestRegisterIntegrationCreatesUserTokenAndEmailJobAtomically proves that a
// successful registration commits the user, its verification token, and the
// outbox email job together.
func TestRegisterIntegrationCreatesUserTokenAndEmailJobAtomically(t *testing.T) {
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := newRegistrationServices(t, pool)

	user, err := svc.Register(ctx, dto.RegisterRequest{
		Email:    "atomic-register@example.com",
		Password: "Password1!",
		Name:     "Atomic Register",
	})
	require.NoError(t, err)
	require.NotZero(t, user.ID)

	var userCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE id = $1 AND deleted_at IS NULL`, user.ID,
	).Scan(&userCount))
	require.Equal(t, 1, userCount, "registration must commit the user")

	var tokenCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM email_verification_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenCount))
	require.Equal(t, 1, tokenCount, "registration must commit exactly one verification token")

	var jobCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs
		 WHERE kind = $1 AND status = 'queued' AND payload->'To' @> jsonb_build_array($2::text)`,
		jobs.EmailJobKind, user.Email,
	).Scan(&jobCount))
	require.Equal(t, 1, jobCount, "registration must enqueue one verification email job")
}

// TestRegisterIntegrationRollsBackUserWhenEnqueueFails drops the outbox table
// so the job insert fails after the user and token writes. Registration must
// fail closed and leave no user behind.
func TestRegisterIntegrationRollsBackUserWhenEnqueueFails(t *testing.T) {
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	pool, cleanup, err := testutil.SetupTestDB(setupCtx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := newRegistrationServices(t, pool)

	const email = "atomic-register-rollback@example.com"
	_, err = pool.Exec(ctx, `DROP TABLE jobs`)
	require.NoError(t, err)

	_, err = svc.Register(ctx, dto.RegisterRequest{
		Email:    email,
		Password: "Password1!",
		Name:     "Atomic Rollback",
	})
	require.Error(t, err, "a failed outbox write must fail registration")
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr, "provisioning failures must use the stable internal error contract")
	require.Equal(t, 500, appErr.Status, "registration provisioning failures must surface as internal errors")

	var userCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE lower(email) = lower($1)`, email,
	).Scan(&userCount))
	require.Zero(t, userCount, "the user insert must roll back when the email cannot be enqueued")

	var tokenCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM email_verification_tokens WHERE lower(email) = lower($1)`, email,
	).Scan(&tokenCount))
	require.Zero(t, tokenCount, "the verification token must roll back with the user")
}
