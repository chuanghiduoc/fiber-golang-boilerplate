//go:build integration

package authn_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// The security notification's encrypted email job is written on the change's
// transaction: it commits with the change and disappears when a later step
// (the audit row) fails and the transaction rolls back.
func TestChangePassword_NotificationJobRollsBackWithChange(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "job-notify@example.com",
		testutil.MemberOptions{Password: testPassphrase, Role: core.RoleMember})

	queue := jobs.NewQueue(e.Fx.Scoped())
	codec, err := jobs.NewPayloadCodec("identity-notification-encryption-key")
	require.NoError(t, err)
	sender := jobs.NewQueuedSender(queue, codec)
	security := authn.NewSecurityNotifier(e.Stores.Tenants, nil)
	newUsers := func(tx *identityrepo.Transactor) authn.UserService {
		return authn.NewUserService(authn.UserDeps{
			Stores: e.Stores, Tx: tx, Cache: cache.NewMemoryCache(), Security: security,
		})
	}
	jobCount := func() int64 {
		return scalar[int64](t, e.Pool, `SELECT count(*) FROM jobs WHERE kind = 'email.send.encrypted' AND tenant_id = $1`, tenant.DefaultID)
	}

	okTx := identityrepo.NewTransactorWithQueue(
		e.Fx.Tx(), audit.NewPostgresRecorder(e.Fx.Scoped()), sender, nil, queue, testutil.BanHasher(t))
	require.NoError(t, newUsers(okTx).ChangePassword(ctx, member.UserID, authn.ChangePasswordInput{
		CurrentPassword: testPassphrase, NewPassword: testNewPassword,
	}))
	require.Equal(t, int64(1), jobCount(), "the email job must commit with the password change")

	// The audit recorder is nil, so the Tier 1 audit write fails after the
	// notification was enqueued; the whole transaction, including the job,
	// must roll back.
	brokenTx := identityrepo.NewTransactorWithQueue(
		e.Fx.Tx(), nil, sender, nil, queue, testutil.BanHasher(t))
	err = newUsers(brokenTx).ChangePassword(ctx, member.UserID, authn.ChangePasswordInput{
		CurrentPassword: testNewPassword, NewPassword: testAnotherPassword,
	})
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), pgfixtures.AppCode(err))
	assert.Equal(t, int64(1), jobCount(), "a rolled-back change must not leave a notification job")
}
