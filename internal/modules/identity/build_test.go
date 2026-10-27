package identity_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identity "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity"
)

// TestBuildRequiresEmailOutbox keeps the post-commit fallbacks from creeping
// back: identity cannot be constructed without the transactional outbox.
func TestBuildRequiresEmailOutbox(t *testing.T) {
	_, err := identity.Build(app.Infra{}, identity.Settings{}, identity.Deps{})
	require.ErrorContains(t, err, "transactional email outbox")
}
