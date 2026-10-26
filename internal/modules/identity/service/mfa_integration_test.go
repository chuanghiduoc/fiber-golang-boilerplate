//go:build integration

package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/totp"
)

// testSealer is a reversible test double for the data-encryption codec; the
// database guards under test do not depend on the ciphertext format.
type testSealer struct{}

// mfaIntegrationPassword is the local password of the MFA integration
// fixtures, used to re-authenticate enrollment management.
var mfaIntegrationPassword = testPassphrase

func (testSealer) Seal(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }

func (testSealer) Open(p []byte) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(p), "sealed:")), nil
}

func enrollSecret(t *testing.T, e *env, userID int64) string {
	t.Helper()
	enrollment, err := e.mfa.EnrollTOTP(tenant.WithID(context.Background(), tenant.DefaultID), userID, 0, mfaIntegrationPassword)
	require.NoError(t, err)
	u, err := url.Parse(enrollment.OtpauthURI)
	require.NoError(t, err)
	secret := u.Query().Get("secret")
	require.NotEmpty(t, secret)
	return secret
}

func recoveryHash(code string) string {
	normalized := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

// One RFC 6238 step may be consumed at most once, even by concurrent requests.
// A failed second-factor guess is audited in its own transaction, so the row
// is visible after the login request returns 401, attributed to the user and
// without the attempted code.
func TestMFAVerifyFailureIsPersistedInAudit(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "mfa-audit@example.com",
		testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)

	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
	require.NoError(t, err)

	challenge, err := e.mfa.BeginChallenge(ctx, user.UserID, service.AMRPassword)
	require.NoError(t, err)
	const attempted = "not-a-real-code"
	_, err = e.mfa.Verify(ctx, challenge, attempted)
	require.Error(t, err)

	var actorID int64
	var actorType, reason string
	require.NoError(t, e.pool.QueryRow(ctx,
		`SELECT actor_id, actor_type, metadata->>'reason' FROM audit_events
		 WHERE action = 'auth.mfa_verify_failed' ORDER BY id DESC LIMIT 1`).
		Scan(&actorID, &actorType, &reason))
	assert.Equal(t, user.UserID, actorID)
	assert.Equal(t, "user", actorType)
	assert.Equal(t, "invalid_code", reason)

	var leaked int64
	require.NoError(t, e.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE metadata::text LIKE '%' || $1 || '%'`, attempted).
		Scan(&leaked))
	assert.Zero(t, leaked, "the attempted code must never be persisted")
}

func TestConsumeTOTPStep_ConcurrentUseSucceedsOnce(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "step@example.com", testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)

	t0 := time.Now()
	code, err := totp.Code(secret, t0)
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
	require.NoError(t, err)

	step := totp.Step(t0) + 1
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = e.stores.MFA.ConsumeTOTPStep(ctx, user.UserID, step)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, countTrue(results), "exactly one concurrent use of a TOTP step may pass")
}

// A recovery code is single use under concurrency.
func TestUseRecoveryCode_ConcurrentUseSucceedsOnce(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "recovery@example.com", testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)

	t0 := time.Now()
	code, err := totp.Code(secret, t0)
	require.NoError(t, err)
	codes, err := e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
	require.NoError(t, err)
	require.Len(t, codes, 10)

	hash := recoveryHash(codes[0])
	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = e.stores.MFA.UseRecoveryCode(ctx, user.UserID, hash)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, countTrue(results), "exactly one concurrent use of a recovery code may pass")
}

// Two concurrent verifications of one challenge and one TOTP code: only one
// may consume the step, consume the challenge, and receive a session.
func TestMFAVerify_ConcurrentChallengeSucceedsOnce(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "verify@example.com", testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)

	t0 := time.Now()
	first, err := totp.Code(secret, t0)
	require.NoError(t, err)
	_, err = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, first)
	require.NoError(t, err)
	// Confirmation used step(t0); the login uses the next step, still inside
	// the +/-1 acceptance window.
	next, err := totp.Code(secret, t0.Add(totp.Period))
	require.NoError(t, err)

	challenge, err := e.mfa.BeginChallenge(ctx, user.UserID, service.AMRPassword)
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]*service.MFALogin, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = e.mfa.Verify(ctx, challenge, next)
		}()
	}
	wg.Wait()

	succeeded := 0
	var sessionID int64
	for i, err := range errs {
		if err == nil {
			succeeded++
			sessionID = results[i].Tokens.SessionID
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one concurrent challenge verification may pass")
	require.Positive(t, sessionID)

	// The session records the full amr set; a refresh keeps it.
	amr := scalar[[]string](t, e.pool, `SELECT amr FROM sessions WHERE id = $1`, sessionID)
	assert.ElementsMatch(t, []string{service.AMRPassword, service.AMROTP}, amr)
}

// Two concurrent confirmations of the same pending factor: one wins, the other
// sees the already-confirmed factor and rolls back.
func TestMFAConfirm_ConcurrentConfirmSucceedsOnce(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	user := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "confirm@example.com", testutil.MemberOptions{Password: mfaIntegrationPassword})
	secret := enrollSecret(t, e, user.UserID)

	code, err := totp.Code(secret, time.Now())
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = e.mfa.ConfirmTOTP(ctx, user.UserID, 0, mfaIntegrationPassword, code)
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	assert.Equal(t, 1, succeeded, "exactly one concurrent confirmation may pass")
	assert.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM mfa_factors WHERE user_id = $1 AND confirmed_at IS NOT NULL`, user.UserID))
}

func countTrue(values []bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
