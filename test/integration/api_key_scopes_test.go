//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
)

// TestIntegration_APIKeyScopesRoundTrip checks that api_key_scopes rows keep
// the scopes API identical to the former JSONB column: sorted, replaced as a
// set on update, and carried over on rotation. The harness has no transaction
// manager, so each write is the repository's single statement.
func TestIntegration_APIKeyScopesRoundTrip(t *testing.T) {
	h := setupAPIKeyHarness(t)
	ctx := systemTenantCtx(h.tenant)

	created, err := h.svc.Create(ctx, h.user, apikeydto.CreateRequest{
		Name:   "scopes",
		Scopes: []string{"files:write", "files:read", "files:read"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"files:read", "files:write"}, created.Scopes)

	got, err := h.svc.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"files:read", "files:write"}, got.Scopes)

	updated, err := h.svc.Update(ctx, created.ID, apikeydto.UpdateRequest{
		Scopes: []string{"notifications:read", "files:read"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"files:read", "notifications:read"}, updated.Scopes)

	var rows []string
	result, err := h.pool.Query(context.Background(),
		`SELECT permission_key FROM api_key_scopes WHERE api_key_id = $1 ORDER BY permission_key`, created.ID)
	require.NoError(t, err)
	defer result.Close()
	for result.Next() {
		var scope string
		require.NoError(t, result.Scan(&scope))
		rows = append(rows, scope)
	}
	require.NoError(t, result.Err())
	assert.Equal(t, []string{"files:read", "notifications:read"}, rows, "update replaces the scope rows")

	rotated, err := h.svc.Rotate(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"files:read", "notifications:read"}, rotated.Scopes)

	keys, _, err := h.svc.List(ctx, 10, "")
	require.NoError(t, err)
	require.Len(t, keys, 2)
	for _, key := range keys {
		assert.Equal(t, []string{"files:read", "notifications:read"}, key.Scopes, "key %d", key.ID)
	}
}
