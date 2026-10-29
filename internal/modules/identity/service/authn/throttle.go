package authn

import (
	"context"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const emailThrottleWindow = time.Minute

// emailThrottleExceeded records one request for the normalized address. It
// fails closed when the shared counter is unavailable: allowing the request
// would remove the only limit on reset and verification emails. The returned
// error is independent of whether the address exists, so the failure cannot
// become an account-enumeration oracle.
func emailThrottleExceeded(ctx context.Context, store cache.Cache, prefix, emailAddr string) (bool, error) {
	count, err := store.Increment(ctx, emailKey(prefix, emailAddr), emailThrottleWindow)
	if err != nil {
		slog.Error("cache unavailable for email throttle; denying request for safety",
			slog.String("throttle", prefix),
			slog.Any("error", err),
		)
		appErr := apperror.NewCode(apperror.CodeSystemUnavailable, "unable to process request, please try again later")
		appErr.Cause = err
		return false, appErr
	}
	return count > 1, nil
}
