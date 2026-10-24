//go:build integration

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TestLinkGoogleAccountIntegrationNeverReplacesGoogleID proves the SQL guard:
// an account already bound to one Google identity cannot be re-linked to
// another, and the service reports it as 409.
func TestLinkGoogleAccountIntegrationNeverReplacesGoogleID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, identitysqlc.CreateUserParams{Email: "linked@example.com", Name: "Linked"})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`UPDATE users SET google_id = 'google-original', email_verified_at = now() WHERE id = $1`, user.ID)
	require.NoError(t, err)

	_, err = userRepo.LinkGoogleAccount(ctx, identitysqlc.LinkGoogleAccountParams{
		GoogleID: pgtype.Text{String: "google-attacker", Valid: true},
		ID:       user.ID,
	})
	require.ErrorIs(t, err, apperror.ErrNotFound, "a different Google id must match no row")

	relinked, err := userRepo.LinkGoogleAccount(ctx, identitysqlc.LinkGoogleAccountParams{
		GoogleID: pgtype.Text{String: "google-original", Valid: true},
		ID:       user.ID,
	})
	require.NoError(t, err, "re-linking the same Google id is idempotent")
	assert.Equal(t, "google-original", relinked.GoogleID.String)

	svc := NewUserService(userRepo, repository.NewRefreshTokenRepository(pool), false,
		cache.NewMemoryCache(), database.NewTxManager(pool), audit.Noop{})
	_, err = svc.SignInWithGoogle(ctx, GoogleSignIn{GoogleID: "google-attacker", Email: "linked@example.com", Name: "Attacker", AllowSignUp: true})
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, http.StatusConflict, appErr.Status)

	var stored string
	require.NoError(t, pool.QueryRow(ctx, `SELECT google_id FROM users WHERE id = $1`, user.ID).Scan(&stored))
	assert.Equal(t, "google-original", stored)
}

// TestForgotPasswordIntegrationRunsThroughDurableJob proves the request path
// only enqueues a sealed password_reset.request job (for known and unknown
// addresses alike) and that the job commits the token with its email job.
func TestForgotPasswordIntegrationRunsThroughDurableJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	userRepo := repository.NewUserRepository(pool)
	user, err := userRepo.Create(ctx, identitysqlc.CreateUserParams{Email: "reset-job@example.com", Name: "Reset Job"})
	require.NoError(t, err)

	queue := jobs.NewQueue(pool)
	codec, err := jobs.NewPayloadCodec("reset-job-integration-secret-0123456789")
	require.NoError(t, err)
	svc := NewPasswordResetService(userRepo, repository.NewPasswordResetRepository(pool),
		repository.NewRefreshTokenRepository(pool), jobs.NewQueuedSender(queue), cache.NewMemoryCache(),
		"http://localhost:3000", database.NewTxManager(pool))
	AttachResetRequestQueue(svc, queue, codec)

	reqCtx := tenant.WithIdentity(ctx, tenant.DefaultID, tenant.DefaultSlug)
	require.NoError(t, svc.ForgotPassword(reqCtx, dto.ForgotPasswordRequest{Email: user.Email}))
	require.NoError(t, svc.ForgotPassword(reqCtx, dto.ForgotPasswordRequest{Email: "nobody@example.com"}))

	countTokens := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1`, user.ID).Scan(&n))
		return n
	}
	require.Zero(t, countTokens(), "the request path must not touch the account")

	rows, err := pool.Query(ctx, `SELECT tenant_id, payload FROM jobs WHERE kind = $1 ORDER BY id`, ResetRequestJobKind)
	require.NoError(t, err)
	type queued struct {
		tenantID int64
		payload  json.RawMessage
	}
	var requests []queued
	for rows.Next() {
		var q queued
		require.NoError(t, rows.Scan(&q.tenantID, &q.payload))
		requests = append(requests, q)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Len(t, requests, 2, "known and unknown addresses enqueue the same job")
	for _, q := range requests {
		assert.Equal(t, tenant.DefaultID, q.tenantID)
		assert.False(t, strings.Contains(string(q.payload), "@example.com"), "the address is sealed at rest")
	}

	for _, q := range requests {
		require.NoError(t, svc.HandleResetRequestJob(tenant.WithID(ctx, q.tenantID), q.payload))
	}
	assert.Equal(t, 1, countTokens())
	var emailJobs int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = $1 AND payload->'To' @> jsonb_build_array($2::text)`,
		jobs.EmailJobKind, user.Email,
	).Scan(&emailJobs))
	assert.Equal(t, 1, emailJobs, "the email job commits with the token")
}

// TestIdentitySchemaIntegrationHardening covers the token-table composite FKs
// and the banned-email lookup index.
func TestIdentitySchemaIntegrationHardening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var otherTenant, userID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('schema-other', 'Other') RETURNING id`).Scan(&otherTenant))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, name) VALUES ('schema@example.com', 'Schema') RETURNING id`).Scan(&userID))

	for _, stmt := range []string{
		`INSERT INTO refresh_tokens (tenant_id, user_id, token, expires_at) VALUES ($1, $2, 'rt-x', now() + interval '1 day')`,
		`INSERT INTO password_reset_tokens (tenant_id, user_id, token, expires_at) VALUES ($1, $2, 'pr-x', now() + interval '1 hour')`,
		`INSERT INTO email_verification_tokens (tenant_id, user_id, email, token, expires_at) VALUES ($1, $2, 'schema@example.com', 'ev-x', now() + interval '1 hour')`,
	} {
		_, err := pool.Exec(ctx, stmt, otherTenant, userID)
		require.Error(t, err, "a token whose tenant differs from its user's must be rejected: %s", stmt)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23503", pgErr.Code, "want a foreign-key violation")
	}

	// The cascade stays: tokens are fully owned by their user.
	_, err = pool.Exec(ctx,
		`INSERT INTO refresh_tokens (tenant_id, user_id, token, expires_at) VALUES (1, $1, 'rt-ok', now() + interval '1 day')`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err)
	var remaining int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE user_id = $1`, userID).Scan(&remaining))
	assert.Zero(t, remaining)

	// GetBannedUserByEmail must be index-backed (seq scans disabled so the
	// tiny test table cannot hide a missing index).
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)
	planRows, err := tx.Query(ctx,
		`EXPLAIN SELECT * FROM users WHERE tenant_id = $1 AND lower(email) = lower($2) AND banned_at IS NOT NULL LIMIT 1`,
		int64(1), "banned@example.com")
	require.NoError(t, err)
	var plan strings.Builder
	for planRows.Next() {
		var line string
		require.NoError(t, planRows.Scan(&line))
		plan.WriteString(line + "\n")
	}
	require.NoError(t, planRows.Err())
	planRows.Close()
	assert.Contains(t, plan.String(), "idx_users_tenant_email_lower_banned")
}
