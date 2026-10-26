package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
)

func TestTenantStatusTransitions(t *testing.T) {
	all := []TenantStatus{TenantStatusTrialing, TenantStatusActive, TenantStatusSuspended, TenantStatusPendingDeletion, TenantStatusDeleted}
	allowed := map[TenantStatus][]TenantStatus{
		TenantStatusTrialing:        {TenantStatusActive, TenantStatusSuspended, TenantStatusPendingDeletion},
		TenantStatusActive:          {TenantStatusSuspended, TenantStatusPendingDeletion},
		TenantStatusSuspended:       {TenantStatusActive, TenantStatusPendingDeletion},
		TenantStatusPendingDeletion: {TenantStatusActive, TenantStatusDeleted},
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
	ctx := context.Background()

	pending, err := svc.Transition(ctx, 2, TenantStatusPendingDeletion)
	require.NoError(t, err)
	require.NotNil(t, pending.DeletionScheduledAt)
	assert.WithinDuration(t, time.Now().Add(TenantDeletionGrace), *pending.DeletionScheduledAt, time.Minute)

	restored, err := svc.Transition(ctx, 2, TenantStatusActive)
	require.NoError(t, err)
	assert.Nil(t, restored.DeletionScheduledAt, "cancelling deletion clears the schedule")

	_, err = svc.Transition(ctx, 2, TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err), "active -> deleted skips the grace period")

	_, err = svc.Transition(ctx, 1, TenantStatusPendingDeletion)
	assert.Equal(t, http.StatusConflict, statusOf(err), "the default tenant cannot be deleted")

	require.Len(t, db.audits, 2)
	assert.Equal(t, "tenant.status_changed", db.audits[0].Action)
	assert.Equal(t, int64(2), db.audits[0].TenantID)
}

func TestTenantTransition_RefusesDeletionBeforeGrace(t *testing.T) {
	db := newMemDB()
	svc := NewTenantService(db.stores(), memTx{db})
	ctx := context.Background()

	future := time.Now().Add(time.Hour)
	db.tenants[2].Status = TenantStatusPendingDeletion
	db.tenants[2].DeletionScheduledAt = &future
	_, err := svc.Transition(ctx, 2, TenantStatusDeleted)
	assert.Equal(t, http.StatusConflict, statusOf(err))
	assert.Equal(t, string(identitycatalog.CodeTenantDeletionNotDue), codeOf(err))

	past := time.Now().Add(-time.Minute)
	db.tenants[2].DeletionScheduledAt = &past
	deleted, err := svc.Transition(ctx, 2, TenantStatusDeleted)
	require.NoError(t, err)
	assert.Equal(t, TenantStatusDeleted, deleted.Status)
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
