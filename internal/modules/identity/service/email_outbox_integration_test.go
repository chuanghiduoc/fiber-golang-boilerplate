//go:build integration

package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TestPasswordResetIntegrationEnqueuesEmailInTokenTransaction proves the reset
// token and the email outbox row commit together.
func TestPasswordResetIntegrationEnqueuesEmailInTokenTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email: "reset-outbox@example.com",
		Name:  "Reset Outbox",
	})
	require.NoError(t, err)

	svc := NewPasswordResetService(
		userRepo,
		repository.NewPasswordResetRepository(pool),
		repository.NewRefreshTokenRepository(pool),
		jobs.NewQueuedSender(jobs.NewQueue(pool)),
		cache.NewMemoryCache(),
		"http://localhost:3000",
		database.NewTxManager(pool),
	)

	require.NoError(t, svc.ForgotPassword(ctx, dto.ForgotPasswordRequest{
		Email: user.Email,
	}))

	var tokenCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenCount))
	require.Equal(t, 1, tokenCount, "reset token must be committed")

	var jobCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs
		 WHERE kind = $1 AND status = 'queued' AND payload->'To' @> jsonb_build_array($2::text)`,
		jobs.EmailJobKind, user.Email,
	).Scan(&jobCount))
	require.Equal(t, 1, jobCount, "password reset email must be queued in the same transaction")
}

// TestPasswordResetIntegrationRollsBackTokenWhenEnqueueFails drops the outbox
// table so the enqueue fails after the token insert. The transaction must roll
// back and leave no usable reset token behind.
func TestPasswordResetIntegrationRollsBackTokenWhenEnqueueFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email: "reset-rollback@example.com",
		Name:  "Reset Rollback",
	})
	require.NoError(t, err)

	// The service must not be able to write the outbox row.
	_, err = pool.Exec(ctx, `DROP TABLE jobs`)
	require.NoError(t, err)

	svc := NewPasswordResetService(
		userRepo,
		repository.NewPasswordResetRepository(pool),
		repository.NewRefreshTokenRepository(pool),
		jobs.NewQueuedSender(jobs.NewQueue(pool)),
		cache.NewMemoryCache(),
		"http://localhost:3000",
		database.NewTxManager(pool),
	)

	err = svc.ForgotPassword(ctx, dto.ForgotPasswordRequest{
		Email: user.Email,
	})
	require.Error(t, err, "enqueue failure must surface instead of leaving a dangling token")

	var tokenCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenCount))
	require.Zero(t, tokenCount, "token insert must roll back when the email cannot be enqueued")
}

// TestEmailVerificationIntegrationEnqueuesEmailInTokenTransaction proves the
// verification token and the email outbox row commit together.
func TestEmailVerificationIntegrationEnqueuesEmailInTokenTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email: "verify-outbox@example.com",
		Name:  "Verify Outbox",
	})
	require.NoError(t, err)

	// Keep a pending token so the test also covers the delete-then-create path.
	verifRepo := repository.NewEmailVerificationRepository(pool)
	_, err = verifRepo.Create(ctx, sqlc.CreateEmailVerificationTokenParams{
		UserID:    user.ID,
		Email:     user.Email,
		Token:     "old-token-hash",
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	require.NoError(t, err)

	svc := NewEmailVerificationService(
		userRepo,
		verifRepo,
		jobs.NewQueuedSender(jobs.NewQueue(pool)),
		cache.NewMemoryCache(),
		"http://localhost:3000",
		database.NewTxManager(pool),
	)

	require.NoError(t, svc.SendVerification(ctx, user.ID, user.Email))

	var (
		tokenCount int
		token      string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), min(token) FROM email_verification_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenCount, &token))
	require.Equal(t, 1, tokenCount, "exactly one verification token must be committed")
	require.NotEqual(t, "old-token-hash", token, "old token must be replaced in the transaction")

	var jobCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs
		 WHERE kind = $1 AND status = 'queued' AND payload->'To' @> jsonb_build_array($2::text)`,
		jobs.EmailJobKind, user.Email,
	).Scan(&jobCount))
	require.Equal(t, 1, jobCount, "verification email must be queued in the same transaction")
}

// TestEmailVerificationIntegrationRollsBackTokenWhenEnqueueFails drops the
// outbox table so the enqueue fails after the old token delete and the new
// token insert. All three steps must roll back together, leaving the old token
// intact and surfacing the error.
func TestEmailVerificationIntegrationRollsBackTokenWhenEnqueueFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, sqlc.CreateUserParams{
		Email: "verify-rollback@example.com",
		Name:  "Verify Rollback",
	})
	require.NoError(t, err)

	verifRepo := repository.NewEmailVerificationRepository(pool)
	const oldToken = "old-token-hash"
	_, err = verifRepo.Create(ctx, sqlc.CreateEmailVerificationTokenParams{
		UserID:    user.ID,
		Email:     user.Email,
		Token:     oldToken,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	require.NoError(t, err)

	// The service must not be able to write the outbox row.
	_, err = pool.Exec(ctx, `DROP TABLE jobs`)
	require.NoError(t, err)

	svc := NewEmailVerificationService(
		userRepo,
		verifRepo,
		jobs.NewQueuedSender(jobs.NewQueue(pool)),
		cache.NewMemoryCache(),
		"http://localhost:3000",
		database.NewTxManager(pool),
	)

	err = svc.SendVerification(ctx, user.ID, user.Email)
	require.Error(t, err, "enqueue failure must surface instead of committing a token")

	var (
		tokenCount int
		token      string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), min(token) FROM email_verification_tokens WHERE user_id = $1`, user.ID,
	).Scan(&tokenCount, &token))
	require.Equal(t, 1, tokenCount, "delete and create must roll back together")
	require.Equal(t, oldToken, token, "the pre-existing token must remain after rollback")
}
