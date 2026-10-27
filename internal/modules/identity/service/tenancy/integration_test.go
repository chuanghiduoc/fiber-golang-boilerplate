//go:build integration

package tenancy_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestTenantService_CreateAndLifecycle(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	tenants := tenancy.NewTenantService(e.Stores, e.Tx)
	trialEnd := time.Now().Add(14 * 24 * time.Hour)

	created, err := tenants.Create(ctx, core.NewTenant{Slug: "trial-co", Name: "Trial Co", Status: core.TenantStatusTrialing, TrialEndsAt: &trialEnd, Timezone: "Asia/Ho_Chi_Minh"})
	require.NoError(t, err)
	settings, err := tenants.Settings(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "Asia/Ho_Chi_Minh", settings.Timezone)
	assert.Equal(t, "en", settings.Locale)
	_, err = tenants.Create(ctx, core.NewTenant{Slug: "trial-co", Name: "Dup", Status: core.TenantStatusActive})
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err))

	// Trialing tenants serve requests; suspended ones do not.
	_, err = e.Stores.Tenants.GetOperationalBySlug(ctx, "trial-co")
	require.NoError(t, err)
	_, err = tenants.Transition(ctx, created.ID, core.TenantStatusSuspended)
	require.NoError(t, err)
	_, err = e.Stores.Tenants.GetOperationalBySlug(ctx, "trial-co")
	assert.ErrorIs(t, err, apperror.ErrNotFound)

	_, err = tenants.Transition(ctx, created.ID, core.TenantStatusPendingDeletion)
	require.NoError(t, err)
	_, err = tenants.Transition(ctx, created.ID, core.TenantStatusDeleted)
	assert.Equal(t, string(identitycatalog.CodeTenantDeletionNotDue), pgfixtures.AppCode(err),
		"deletion is refused until the grace period ends")
	if _, err := e.Pool.Exec(ctx,
		`UPDATE tenants SET deletion_scheduled_at = NOW() - interval '1 minute' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("move deletion schedule into the past: %v", err)
	}
	deleted, err := tenants.Transition(ctx, created.ID, core.TenantStatusDeleted)
	require.NoError(t, err)
	assert.NotNil(t, deleted.DeletionScheduledAt, "the schedule is kept as history")
	_, err = tenants.Transition(ctx, created.ID, core.TenantStatusActive)
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err), "deleted is terminal")
	assert.Equal(t, int64(4), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM audit_events WHERE resource = 'tenant' AND resource_id = $1`, created.ID.String()))

	// The schema backs the state machine's invariants.
	_, err = e.Pool.Exec(ctx, `UPDATE tenants SET status = 'deleted', deletion_scheduled_at = NOW() WHERE id = '0192c9b0-7e4a-7000-8000-000000000001'`)
	assert.Error(t, err, "the default tenant can never be deleted")
	_, err = e.Pool.Exec(ctx, `UPDATE tenants SET status = 'trialing', trial_ends_at = NULL WHERE id = '0192c9b0-7e4a-7000-8000-000000000001'`)
	assert.Error(t, err, "a trialing tenant needs a trial end")

	baseline, configured, err := tenancy.StartupTenants(ctx, e.Stores.Tenants, "trial-co")
	require.NoError(t, err, "a deleted configured tenant still exists for startup checks")
	assert.Equal(t, testID(1), baseline.ID)
	assert.Equal(t, created.ID, configured.ID)
	_, _, err = tenancy.StartupTenants(ctx, e.Stores.Tenants, "missing")
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}
