package features

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Require returns nil when key is enabled for subject and a 404
// feature.disabled error otherwise. A nil evaluator uses only compiled
// defaults, so an unwired caller fails closed for default-off flags.
func Require(ctx context.Context, flags Evaluator, key Key, subject Subject) error {
	if flags == nil {
		flags = NewEvaluator(Options{})
	}
	if flags.Enabled(ctx, key, subject) {
		return nil
	}
	return apperror.NewCode(apperror.CodeFeatureDisabled, fmt.Sprintf("feature %s is disabled", key))
}

// Middleware gates a route on key for the request tenant and, when the route
// is authenticated, the JWT user. Mount it after tenant resolution and
// authentication (so user targets apply) and before idempotency/audit, so a
// disabled request neither caches a response nor writes an audit intent row.
func Middleware(flags Evaluator, key Key) fiber.Handler {
	return func(c fiber.Ctx) error {
		if err := Require(c.Context(), flags, key, SubjectFromContext(c.Context(), httpx.AuthUserID(c))); err != nil {
			return err
		}
		return c.Next()
	}
}
