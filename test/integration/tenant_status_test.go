//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

func TestTenantStatus_SuspendedDefaultTenantReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var suspendedID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name, status)
		 VALUES ('suspended-default', 'Suspended Default', 'suspended') RETURNING id`,
	).Scan(&suspendedID))
	require.NotZero(t, suspendedID)

	resolver := identityadapter.NewTenantResolver(identityrepo.NewTenantRepository(pool))
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	api := app.Group("/api/v1", tenanthttp.Middleware(resolver, "suspended-default"))
	api.Get("/ping", func(c fiber.Ctx) error { return c.SendString("pong") })

	req, err := http.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
	require.NoError(t, err)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	// An explicit header for the same suspended tenant is also rejected.
	req, err = http.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(tenant.Header, "suspended-default")
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}
