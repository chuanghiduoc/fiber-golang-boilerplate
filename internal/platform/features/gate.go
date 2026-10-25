package features

import (
	"context"
	"fmt"

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
