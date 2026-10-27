package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeUserEvents struct {
	events []audit.UserEvent
}

func (f fakeUserEvents) ListUserEvents(context.Context, uuid.UUID, uuid.UUID, int32) ([]audit.UserEvent, error) {
	return f.events, nil
}

func TestListErasureRequests_DerivesStatusFromAuditAndMembership(t *testing.T) {
	db := fakes.New()
	ctx := fakes.TenantCtx()
	userID := fakes.TestID(10)
	requestedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	db.Users[userID] = &core.User{ID: userID, Email: "erase@example.com", Name: "Erase", Status: core.UserStatusActive}
	db.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &core.Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: core.RoleMember,
		Status: core.MembershipStatusSuspended, ErasureRequestedAt: &requestedAt,
	}

	svc := NewPrivacyService(PrivacyDeps{
		Stores: db.Stores(),
		AuditEvents: fakeUserEvents{events: []audit.UserEvent{
			{Action: "user.login"},
			{Action: "user.erasure_requested", CreatedAt: requestedAt},
		}},
	})

	requests, hasMore, err := svc.ListErasureRequests(ctx, userID, 20, "")
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, requests, 1)
	assert.Equal(t, userID, requests[0].ID)
	assert.Equal(t, core.ErasureScopeSelf, requests[0].Scope)
	assert.Equal(t, core.ErasureProcessing, requests[0].Status)
	assert.Equal(t, requestedAt, requests[0].RequestedAt)

	fetched, err := svc.GetErasureRequest(ctx, userID, userID)
	require.NoError(t, err)
	assert.Equal(t, core.ErasureProcessing, fetched.Status)

	_, err = svc.GetErasureRequest(ctx, userID, fakes.TestID(99))
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

func TestListErasureRequests_CompletedAfterMembershipRemoval(t *testing.T) {
	db := fakes.New()
	ctx := fakes.TenantCtx()
	userID := fakes.TestID(11)
	requestedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	removedAt := requestedAt.Add(time.Hour)
	db.Users[userID] = &core.User{ID: userID, Email: "done@example.com", Name: "Done", Status: core.UserStatusActive}
	db.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &core.Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: core.RoleMember,
		Status: core.MembershipStatusRemoved, RemovedAt: &removedAt,
	}

	svc := NewPrivacyService(PrivacyDeps{
		Stores:      db.Stores(),
		AuditEvents: fakeUserEvents{events: []audit.UserEvent{{Action: "user.erasure_requested", CreatedAt: requestedAt}}},
	})
	requests, _, err := svc.ListErasureRequests(ctx, userID, 20, "")
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, core.ErasureCompleted, requests[0].Status)
	require.NotNil(t, requests[0].CompletedAt)
	assert.Equal(t, removedAt, *requests[0].CompletedAt)
}

func TestListErasureRequests_NoAuditRowIsEmpty(t *testing.T) {
	db := fakes.New()
	svc := NewPrivacyService(PrivacyDeps{Stores: db.Stores(), AuditEvents: fakeUserEvents{}})
	requests, hasMore, err := svc.ListErasureRequests(fakes.TenantCtx(), fakes.TestID(12), 20, "")
	require.NoError(t, err)
	assert.False(t, hasMore)
	assert.Empty(t, requests)
}
