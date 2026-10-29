package privacy

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeUserEvents struct {
	events       []audit.UserEvent
	targetEvents []audit.UserEvent
	truncated    bool
}

func (f fakeUserEvents) ListUserEvents(context.Context, uuid.UUID, uuid.UUID, int32) (audit.UserEventPage, error) {
	return audit.UserEventPage{Events: f.events, Truncated: f.truncated}, nil
}

func (f fakeUserEvents) LatestUserActorEvent(_ context.Context, _, _ uuid.UUID, action string) (audit.UserEvent, bool, error) {
	for _, event := range f.events {
		if event.Action == action {
			return event, true, nil
		}
	}
	return audit.UserEvent{}, false, nil
}

func (f fakeUserEvents) LatestUserTargetEvent(_ context.Context, _, _ uuid.UUID, action string) (audit.UserEvent, bool, error) {
	for _, event := range f.targetEvents {
		if event.Action == action {
			return event, true, nil
		}
	}
	return audit.UserEvent{}, false, nil
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

func TestGetMemberErasureRequest_DerivesTenantScopedAdminStatus(t *testing.T) {
	db := fakes.New()
	ctx := adminCallerCtx(db)
	userID := fakes.TestID(21)
	requestedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	db.Users[userID] = &core.User{ID: userID, Email: "member@example.com", Name: "Member", Status: core.UserStatusActive}
	db.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &core.Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: core.RoleMember,
		Status: core.MembershipStatusActive, ErasureRequestedAt: &requestedAt,
	}

	svc := NewPrivacyService(PrivacyDeps{
		Stores: db.Stores(),
		// The admin is the actor, so the status lookup must read by resource.
		AuditEvents: fakeUserEvents{targetEvents: []audit.UserEvent{
			{Action: "admin.user.erasure_requested", CreatedAt: requestedAt},
		}},
	})

	request, err := svc.GetMemberErasureRequest(ctx, userID, userID)
	require.NoError(t, err)
	assert.Equal(t, userID, request.ID)
	assert.Equal(t, userID, request.UserID)
	assert.Equal(t, core.ErasureScopeMember, request.Scope)
	assert.Equal(t, core.ErasureProcessing, request.Status)
	assert.Equal(t, requestedAt, request.RequestedAt)

	_, err = svc.GetMemberErasureRequest(ctx, userID, fakes.TestID(99))
	assert.ErrorIs(t, err, apperror.ErrNotFound)

	noRequest := NewPrivacyService(PrivacyDeps{Stores: db.Stores(), AuditEvents: fakeUserEvents{}})
	_, err = noRequest.GetMemberErasureRequest(ctx, userID, userID)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

// An admin may request erasure only for members below their rank, so reading
// that request status follows the same rule.
func TestGetMemberErasureRequest_RequiresManageRank(t *testing.T) {
	db := fakes.New()
	ctx := adminCallerCtx(db)
	userID := fakes.TestID(22)
	requestedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	db.Users[userID] = &core.User{ID: userID, Email: "owner@example.com", Name: "Owner", Status: core.UserStatusActive}
	db.Memberships[[2]uuid.UUID{tenant.IDFromContext(ctx), userID}] = &core.Membership{
		TenantID: tenant.IDFromContext(ctx), UserID: userID, Role: core.RoleOwner, Roles: []string{core.RoleOwner},
		Status: core.MembershipStatusActive, ErasureRequestedAt: &requestedAt,
	}
	svc := NewPrivacyService(PrivacyDeps{
		Stores: db.Stores(),
		AuditEvents: fakeUserEvents{targetEvents: []audit.UserEvent{
			{Action: "admin.user.erasure_requested", CreatedAt: requestedAt},
		}},
	})

	_, err := svc.GetMemberErasureRequest(ctx, userID, userID)
	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appErr.Code)
}

// A finished erasure deleted the membership, so there is no rank left to
// compare: the status row still reports the request as completed.
func TestGetMemberErasureRequest_CompletedAfterMembershipDeletion(t *testing.T) {
	db := fakes.New()
	ctx := adminCallerCtx(db)
	userID := fakes.TestID(23)
	requestedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	delete(db.Memberships, [2]uuid.UUID{tenant.IDFromContext(ctx), userID})
	svc := NewPrivacyService(PrivacyDeps{
		Stores: db.Stores(),
		AuditEvents: fakeUserEvents{targetEvents: []audit.UserEvent{
			{Action: "admin.user.erasure_requested", CreatedAt: requestedAt},
		}},
	})

	request, err := svc.GetMemberErasureRequest(ctx, userID, userID)
	require.NoError(t, err)
	assert.Equal(t, core.ErasureCompleted, request.Status)
	require.NotNil(t, request.CompletedAt)
}
