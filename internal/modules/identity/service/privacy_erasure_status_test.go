package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	db := newMemDB()
	ctx := testTenantCtx()
	userID := testID(10)
	requestedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	db.users[userID] = &User{ID: userID, Email: "erase@example.com", Name: "Erase", Status: UserStatusActive}
	db.memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: RoleMember,
		Status: MembershipStatusSuspended, ErasureRequestedAt: &requestedAt,
	}

	svc := NewPrivacyService(PrivacyDeps{
		Stores: db.stores(),
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
	assert.Equal(t, ErasureScopeSelf, requests[0].Scope)
	assert.Equal(t, ErasureProcessing, requests[0].Status)
	assert.Equal(t, requestedAt, requests[0].RequestedAt)

	fetched, err := svc.GetErasureRequest(ctx, userID, userID)
	require.NoError(t, err)
	assert.Equal(t, ErasureProcessing, fetched.Status)

	_, err = svc.GetErasureRequest(ctx, userID, testID(99))
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

func TestListErasureRequests_CompletedAfterMembershipRemoval(t *testing.T) {
	db := newMemDB()
	ctx := testTenantCtx()
	userID := testID(11)
	requestedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	removedAt := requestedAt.Add(time.Hour)
	db.users[userID] = &User{ID: userID, Email: "done@example.com", Name: "Done", Status: UserStatusActive}
	db.memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: RoleMember,
		Status: MembershipStatusRemoved, RemovedAt: &removedAt,
	}

	svc := NewPrivacyService(PrivacyDeps{
		Stores:      db.stores(),
		AuditEvents: fakeUserEvents{events: []audit.UserEvent{{Action: "user.erasure_requested", CreatedAt: requestedAt}}},
	})
	requests, _, err := svc.ListErasureRequests(ctx, userID, 20, "")
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, ErasureCompleted, requests[0].Status)
	require.NotNil(t, requests[0].CompletedAt)
	assert.Equal(t, removedAt, *requests[0].CompletedAt)
}

func TestListErasureRequests_NoAuditRowIsEmpty(t *testing.T) {
	db := newMemDB()
	svc := NewPrivacyService(PrivacyDeps{Stores: db.stores(), AuditEvents: fakeUserEvents{}})
	requests, hasMore, err := svc.ListErasureRequests(testTenantCtx(), testID(12), 20, "")
	require.NoError(t, err)
	assert.False(t, hasMore)
	assert.Empty(t, requests)
}
