package authn

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type failingIncrementCache struct{ cache.Cache }

func (failingIncrementCache) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("cache unavailable")
}

func newFailingIncrementCache(t *testing.T) cache.Cache {
	t.Helper()
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })
	return failingIncrementCache{Cache: mem}
}

func TestPasswordResetThrottleFailsClosed(t *testing.T) {
	db := fakes.New()
	svc := NewPasswordResetService(ResetDeps{
		Stores: db.Stores(),
		Tx:     fakes.Tx{DB: db},
		Cache:  newFailingIncrementCache(t),
	})

	for _, emailAddr := range []string{"known@example.com", "unknown@example.com"} {
		err := svc.ForgotPassword(fakes.TenantCtx(), ForgotPasswordInput{Email: emailAddr})
		requireThrottleUnavailable(t, err)
	}
}

func TestVerificationResendThrottleFailsClosed(t *testing.T) {
	db := fakes.New()
	svc := NewEmailVerificationService(VerificationDeps{
		Stores: db.Stores(),
		Tx:     fakes.Tx{DB: db},
		Cache:  newFailingIncrementCache(t),
	})

	for _, emailAddr := range []string{"known@example.com", "unknown@example.com"} {
		err := svc.ResendVerification(fakes.TenantCtx(), emailAddr)
		requireThrottleUnavailable(t, err)
	}
}

func requireThrottleUnavailable(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, string(apperror.CodeSystemUnavailable), appErr.Code)
	require.Error(t, appErr.Cause)
	problem := apperror.NewCatalog().Problem(appErr, "test-request", "test-instance")
	require.Equal(t, http.StatusServiceUnavailable, problem.Status)
}
