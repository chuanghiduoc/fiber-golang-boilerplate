package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
)

func TestTenantStatusTransitions(t *testing.T) {
	all := []TenantStatus{TenantStatusTrialing, TenantStatusActive, TenantStatusSuspended, TenantStatusPendingDeletion, TenantStatusDeleted}
	allowed := map[TenantStatus][]TenantStatus{
		TenantStatusTrialing:        {TenantStatusActive, TenantStatusSuspended, TenantStatusPendingDeletion},
		TenantStatusActive:          {TenantStatusSuspended, TenantStatusPendingDeletion},
		TenantStatusSuspended:       {TenantStatusActive, TenantStatusPendingDeletion},
		TenantStatusPendingDeletion: {TenantStatusSuspended, TenantStatusDeleted},
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
	assert.False(t, TenantStatusTransitionAllowed("bogus", TenantStatusActive), "unknown states fail closed")
}

func TestTenantStatusOperational(t *testing.T) {
	for status, want := range map[TenantStatus]bool{
		TenantStatusTrialing: true, TenantStatusActive: true, TenantStatusSuspended: false,
		TenantStatusPendingDeletion: false, TenantStatusDeleted: false, "bogus": false,
	} {
		assert.Equal(t, want, status.Operational(), string(status))
	}
}

func TestTenantTransition_SchedulesAndAudits(t *testing.T) {
	db := newMemDB()
	svc := NewTenantService(db.stores(), memTx{db})
	ctx := testTenantCtx()

	pending, err := svc.Transition(ctx, testID(2), TenantStatusPendingDeletion)
	require.NoError(t, err)
	require.NotNil(t, pending.DeletionScheduledAt)
	assert.WithinDuration(t, time.Now().Add(TenantDeletionGrace), *pending.DeletionScheduledAt, time.Minute)

	restored, err := svc.Transition(ctx, testID(2), TenantStatusSuspended)
	require.NoError(t, err, "cancelling a deletion suspends the tenant")
	assert.Nil(t, restored.DeletionScheduledAt, "cancelling deletion clears the schedule")

	activated, err := svc.Transition(ctx, testID(2), TenantStatusActive)
	require.NoError(t, err, "a platform admin activates the restored tenant explicitly")
	assert.Equal(t, TenantStatusActive, activated.Status)

	_, err = svc.Transition(ctx, testID(2), TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err), "active -> deleted skips the grace period")

	_, err = svc.Transition(ctx, testID(1), TenantStatusPendingDeletion)
	assert.Equal(t, http.StatusConflict, statusOf(err), "the default tenant cannot be deleted")

	require.Len(t, db.audits, 3)
	assert.Equal(t, "tenant.status_changed", db.audits[0].Action)
	assert.Equal(t, testID(2), db.audits[0].TenantID)
}

func TestTenantTransition_RefusesDeletionBeforeGrace(t *testing.T) {
	db := newMemDB()
	svc := NewTenantService(db.stores(), memTx{db})
	ctx := testTenantCtx()

	future := time.Now().Add(time.Hour)
	db.tenants[testID(2)].Status = TenantStatusPendingDeletion
	db.tenants[testID(2)].DeletionScheduledAt = &future
	_, err := svc.Transition(ctx, testID(2), TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err))
	assert.Equal(t, string(identitycatalog.CodeTenantDeletionNotDue), codeOf(err))

	past := time.Now().Add(-time.Minute)
	db.tenants[testID(2)].DeletionScheduledAt = &past
	deleted, err := svc.Transition(ctx, testID(2), TenantStatusDeleted)
	require.NoError(t, err)
	assert.Equal(t, TenantStatusDeleted, deleted.Status)
}

func TestTenantExpireTrialSuspendsOnlyPastDue(t *testing.T) {
	db := newMemDB()
	svc := NewTenantService(db.stores(), memTx{db})
	ctx := testTenantCtx()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	db.tenants[testID(2)].Status = TenantStatusTrialing
	db.tenants[testID(2)].TrialEndsAt = &past
	db.tenants[testID(3)] = &Tenant{ID: testID(3), Slug: "live", Name: "Live", Status: TenantStatusTrialing, TrialEndsAt: &future}

	changed, err := svc.ExpireTrial(ctx, testID(2))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, TenantStatusSuspended, db.tenants[testID(2)].Status)
	require.Len(t, db.audits, 1)
	assert.Equal(t, "tenant.status_changed", db.audits[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, db.audits[0].ActorType)
	assert.Equal(t, "trial_expired", db.audits[0].Metadata["reason"])

	changed, err = svc.ExpireTrial(ctx, testID(2))
	require.NoError(t, err)
	assert.False(t, changed, "a retry after suspension is a no-op")
	assert.Len(t, db.audits, 1, "a no-op writes no audit row")

	changed, err = svc.ExpireTrial(ctx, testID(3))
	require.NoError(t, err)
	assert.False(t, changed, "a live trial stays trialing")
	assert.Equal(t, TenantStatusTrialing, db.tenants[testID(3)].Status)
}

func TestTenantFinalizeDeletionTransitionsOnlyDueTenants(t *testing.T) {
	db := newMemDB()
	svc := NewTenantService(db.stores(), memTx{db})
	ctx := testTenantCtx()
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	db.tenants[testID(2)].Status = TenantStatusPendingDeletion
	db.tenants[testID(2)].DeletionScheduledAt = &past
	db.tenants[testID(3)] = &Tenant{
		ID: testID(3), Slug: "not-due", Name: "Not Due", Status: TenantStatusPendingDeletion, DeletionScheduledAt: &future,
	}

	changed, err := svc.FinalizeDeletion(ctx, testID(2))
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, TenantStatusDeleted, db.tenants[testID(2)].Status)
	require.Len(t, db.audits, 1)
	assert.Equal(t, "tenant.status_changed", db.audits[0].Action)
	assert.Equal(t, audit.ActorTypeSystem, db.audits[0].ActorType)
	assert.Equal(t, "deletion_due", db.audits[0].Metadata["reason"])

	changed, err = svc.FinalizeDeletion(ctx, testID(2))
	require.NoError(t, err)
	assert.False(t, changed, "a retry after deletion is a no-op")

	changed, err = svc.FinalizeDeletion(ctx, testID(3))
	require.NoError(t, err)
	assert.False(t, changed, "a tenant before its grace end stays pending")
	assert.Equal(t, TenantStatusPendingDeletion, db.tenants[testID(3)].Status)
	assert.Len(t, db.audits, 1, "a no-op writes no audit row")
}

func TestNormalizeNewTenant(t *testing.T) {
	future := time.Now().Add(time.Hour)
	ok := NewTenant{Slug: "acme", Name: "Acme", Status: TenantStatusTrialing, TrialEndsAt: &future}
	require.NoError(t, normalizeNewTenant(&ok))
	assert.Equal(t, defaultTenantLocale, ok.Locale)
	assert.Equal(t, defaultTenantTimezone, ok.Timezone)

	for name, in := range map[string]NewTenant{
		"trial without end": {Status: TenantStatusTrialing},
		"suspended":         {Status: TenantStatusSuspended},
		"bad timezone":      {Status: TenantStatusActive, Timezone: "Mars/Olympus"},
		"bad locale":        {Status: TenantStatusActive, Locale: "en_US"},
	} {
		assert.Error(t, normalizeNewTenant(&in), name)
	}
}
