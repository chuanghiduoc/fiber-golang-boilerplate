//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

// The security notification's email.send job is written on the change's
// transaction: it commits with the change and disappears when a later step
// (the audit row) fails and the transaction rolls back.
func TestChangePassword_NotificationJobRollsBackWithChange(t *testing.T) {
	e := newEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "job-notify@example.com",
		testutil.MemberOptions{Password: "Password1!", Role: identityservice.RoleMember})

	queue := jobs.NewQueue(testutil.Scoped(e.pool))
	sender := jobs.NewQueuedSender(queue)
	security := identityservice.NewSecurityNotifier(e.stores.Tenants, nil)
	newUsers := func(tx *identityrepo.Transactor) identityservice.UserService {
		return identityservice.NewUserService(identityservice.UserDeps{
			Stores: e.stores, Tx: tx, Cache: cache.NewMemoryCache(), Security: security,
		})
	}
	jobCount := func() int64 {
		return scalar[int64](t, e.pool, `SELECT count(*) FROM jobs WHERE kind = 'email.send' AND tenant_id = $1`, tenant.DefaultID)
	}

	okTx := identityrepo.NewTransactorWithQueue(
		testutil.NewTxManager(e.pool), audit.NewPostgresRecorder(testutil.Scoped(e.pool)), sender, nil, queue)
	require.NoError(t, newUsers(okTx).ChangePassword(ctx, member.UserID, identityservice.ChangePasswordInput{
		CurrentPassword: "Password1!", NewPassword: "Password2!",
	}))
	require.Equal(t, int64(1), jobCount(), "the email job must commit with the password change")

	// The audit recorder is nil, so the Tier 1 audit write fails after the
	// notification was enqueued; the whole transaction, including the job,
	// must roll back.
	brokenTx := identityrepo.NewTransactorWithQueue(
		testutil.NewTxManager(e.pool), nil, sender, nil, queue)
	err := newUsers(brokenTx).ChangePassword(ctx, member.UserID, identityservice.ChangePasswordInput{
		CurrentPassword: "Password2!", NewPassword: "Password3!",
	})
	require.Error(t, err)
	assert.Equal(t, string(apperror.CodeSystemAuditUnavailable), appCode(err))
	assert.Equal(t, int64(1), jobCount(), "a rolled-back change must not leave a notification job")
}
