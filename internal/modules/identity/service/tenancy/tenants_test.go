package tenancy

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

func TestTenantStatusTransitions(t *testing.T) {
	all := []core.TenantStatus{core.TenantStatusTrialing, core.TenantStatusActive, core.TenantStatusSuspended, core.TenantStatusPendingDeletion, core.TenantStatusDeleted}
	allowed := map[core.TenantStatus][]core.TenantStatus{
		TenantStatusTrialing:        {core.TenantStatusActive, core.TenantStatusSuspended, core.TenantStatusPendingDeletion},
		TenantStatusActive:          {core.TenantStatusSuspended, core.TenantStatusPendingDeletion},
		TenantStatusSuspended:       {core.TenantStatusActive, core.TenantStatusPendingDeletion},
		TenantStatusPendingDeletion: {core.TenantStatusSuspended, core.TenantStatusDeleted},
		TenantStatusDeleted:         nil,
	}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, ok := range allowed[from] {
				want = want || ok == to
			}
			assert.Equal(t, want, TenantStatusTransitionAllowed(from, to), "%s -> %s", from, to)
		}
	}
	assert.False(t, TenantStatusTransitionAllowed("bogus", core.TenantStatusActive), "unknown states fail closed")
}

func TestTenantStatusOperational(t *testing.T) {
	for status, want := range map[core.TenantStatus]bool{
		TenantStatusTrialing: true, TenantStatusActive: true, TenantStatusSuspended: false,
		TenantStatusPendingDeletion: false, TenantStatusDeleted: false, "bogus": false,
	} {
		assert.Equal(t, want, status.Operational(), string(status))
	}
}

func TestTenantTransition_SchedulesAndAudits(t *testing.T) {
	db := fakes.New()
	svc := NewTenantService(db.Stores(), fakes.Tx{DB: db})
	ctx := fakes.TenantCtx()

	pending, err := svc.Transition(ctx, fakes.TestID(2), core.TenantStatusPendingDeletion)
	require.NoError(t, err)
	require.NotNil(t, pending.DeletionScheduledAt)
	assert.WithinDuration(t, time.Now().Add(TenantDeletionGrace), *pending.DeletionScheduledAt, time.Minute)

	restored, err := svc.Transition(ctx, fakes.TestID(2), core.TenantStatusSuspended)
	require.NoError(t, err, "cancelling a deletion suspends the tenant")
	assert.Nil(t, restored.DeletionScheduledAt, "cancelling deletion clears the schedule")

	activated, err := svc.Transition(ctx, fakes.TestID(2), core.TenantStatusActive)
	require.NoError(t, err, "a platform admin activates the restored tenant explicitly")
	assert.Equal(t, core.TenantStatusActive, activated.Status)

	_, err = svc.Transition(ctx, fakes.TestID(2), core.TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err), "active -> deleted skips the grace period")

	_, err = svc.Transition(ctx, fakes.TestID(1), core.TenantStatusPendingDeletion)
	assert.Equal(t, http.StatusConflict, statusOf(err), "the default tenant cannot be deleted")

	require.Len(t, db.Audits, 3)
	assert.Equal(t, "tenant.status_changed", db.Audits[0].Action)
	assert.Equal(t, fakes.TestID(2), db.Audits[0].TenantID)
}

func TestTenantTransition_RefusesDeletionBeforeGrace(t *testing.T) {
	db := fakes.New()
	svc := NewTenantService(db.Stores(), fakes.Tx{DB: db})
	ctx := fakes.TenantCtx()

	future := time.Now().Add(time.Hour)
	db.Tenants[fakes.TestID(2)].Status = core.TenantStatusPendingDeletion
	db.Tenants[fakes.TestID(2)].DeletionScheduledAt = &future
	_, err := svc.Transition(ctx, fakes.TestID(2), core.TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err))
	assert.Equal(t, string(identitycatalog.CodeTenantDeletionNotDue), codeOf(err))

	past := time.Now().Add(-time.Minute)
	db.Tenants[fakes.TestID(2)].DeletionScheduledAt = &past
	deleted, err := svc.Transition(ctx, fakes.TestID(2), core.TenantStatusDeleted)
	require.NoError(t, err)
	assert.Equal(t, core.TenantStatusDeleted, deleted.Status)
}

func TestTenantExpireTrialSuspendsOnlyPastDue(t *testing.T) {
	db := fakes.New()
	svc := NewTenantService(db.Stores(), fakes.Tx{DB: db})
	ctx := fakes.TenantCtx()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	db.Tenants[fakes.TestID(2)].Status = core.TenantStatusTrialing
	db.Tenants[fakes.TestID(2)].TrialEndsAt = &past
	db.Tenants[fakes.TestID(3)] = &core.Tenant{ID: fakes.TestID(3), Slug: "live", Name: "Live", Status: core.TenantStatusTrialing, TrialEndsAt: &future}

	changed, err := svc.ExpireTrial(ctx, fakes.TestID(2))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, core.TenantStatusSuspended, db.Tenants[fakes.TestID(2)].Status)
	require.Len(t, db.Audits, 1)
	assert.Equal(t, "tenant.status_changed", db.Audits[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, db.Audits[0].ActorType)
	assert.Equal(t, "trial_expired", db.Audits[0].Metadata["reason"])

	changed, err = svc.ExpireTrial(ctx, fakes.TestID(2))
	require.NoError(t, err)
	assert.False(t, changed, "a retry after suspension is a no-op")
	assert.Len(t, db.Audits, 1, "a no-op writes no audit row")

	changed, err = svc.ExpireTrial(ctx, fakes.TestID(3))
	require.NoError(t, err)
	assert.False(t, changed, "a live trial stays trialing")
	assert.Equal(t, core.TenantStatusTrialing, db.Tenants[fakes.TestID(3)].Status)
}

func TestTenantFinalizeDeletionTransitionsOnlyDueTenants(t *testing.T) {
	db := fakes.New()
	svc := NewTenantService(db.Stores(), fakes.Tx{DB: db})
	ctx := fakes.TenantCtx()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	db.Tenants[fakes.TestID(2)].Status = core.TenantStatusPendingDeletion
	db.Tenants[fakes.TestID(2)].DeletionScheduledAt = &past
	db.Tenants[fakes.TestID(3)] = &core.Tenant{
		ID: fakes.TestID(3), Slug: "not-due", Name: "Not Due", Status: core.TenantStatusPendingDeletion, DeletionScheduledAt: &future,
	}

	changed, err := svc.FinalizeDeletion(ctx, fakes.TestID(2))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, core.TenantStatusDeleted, db.Tenants[fakes.TestID(2)].Status)
	require.Len(t, db.Audits, 1)
	assert.Equal(t, "tenant.status_changed", db.Audits[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, db.Audits[0].ActorType)
	assert.Equal(t, "deletion_due", db.Audits[0].Metadata["reason"])

	changed, err = svc.FinalizeDeletion(ctx, fakes.TestID(2))
	require.NoError(t, err)
	assert.False(t, changed, "a retry after deletion is a no-op")

	changed, err = svc.FinalizeDeletion(ctx, fakes.TestID(3))
	require.NoError(t, err)
	assert.False(t, changed, "a tenant before its grace end stays pending")
	assert.Equal(t, core.TenantStatusPendingDeletion, db.Tenants[fakes.TestID(3)].Status)
	assert.Len(t, db.Audits, 1, "a no-op writes no audit row")
}

func TestNormalizeNewTenant(t *testing.T) {
	future := time.Now().Add(time.Hour)
	ok := core.NewTenant{Slug: "acme", Name: "Acme", Status: core.TenantStatusTrialing, TrialEndsAt: &future}
	require.NoError(t, normalizeNewTenant(&ok))
	assert.Equal(t, defaultTenantLocale, ok.Locale)
	assert.Equal(t, defaultTenantTimezone, ok.Timezone)

	for name, in := range map[string]core.NewTenant{
		"trial without end": {Status: core.TenantStatusTrialing},
		"suspended":         {Status: core.TenantStatusSuspended},
		"bad timezone":      {Status: core.TenantStatusActive, Timezone: "Mars/Olympus"},
		"bad locale":        {Status: core.TenantStatusActive, Locale: "en_US"},
	} {
		assert.Error(t, normalizeNewTenant(&in), name)
	}
}
