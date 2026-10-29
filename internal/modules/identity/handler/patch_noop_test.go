package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
)

type memberNoOpAdmin struct {
	MemberAdminService
	member  *core.Member
	updates int
}

func (s *memberNoOpAdmin) GetMember(context.Context, uuid.UUID) (*core.Member, error) {
	return s.member, nil
}

func (s *memberNoOpAdmin) UpdateMember(context.Context, uuid.UUID, membership.MemberUpdate) (*core.Member, error) {
	s.updates++
	return s.member, nil
}

func TestUpdateMemberEmptyPatchIsNoOp(t *testing.T) {
	updatedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	member := &core.Member{
		User:       core.User{ID: testID(1), Name: "Member", UpdatedAt: updatedAt},
		Membership: core.Membership{TenantID: testID(2), Status: core.MembershipStatusActive, CreatedAt: updatedAt, UpdatedAt: updatedAt},
	}
	svc := &memberNoOpAdmin{member: member}
	handler := NewMemberHandler(svc, nil, nil)

	out, err := handler.Update(context.Background(), &updateMemberInput{
		MemberID: testID(1).String(),
		IfMatch:  etagHeader(updatedAt),
		Body:     dto.MemberPatch{},
	})
	require.NoError(t, err)
	require.Zero(t, svc.updates, "an empty merge patch must not update or audit the member")
	require.Equal(t, etagHeader(updatedAt), out.ETag)
	require.Equal(t, testID(1), out.Body.ID)
}

type tenantNoOpAdmin struct {
	TenantAdmin
	tenant  *core.Tenant
	updates int
}

func (s *tenantNoOpAdmin) Get(context.Context, uuid.UUID) (*core.Tenant, error) {
	return s.tenant, nil
}

func (s *tenantNoOpAdmin) Update(context.Context, uuid.UUID, core.TenantUpdate) (*core.Tenant, error) {
	s.updates++
	return s.tenant, nil
}

func TestUpdateTenantEmptyPatchIsNoOp(t *testing.T) {
	updatedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tenant := &core.Tenant{ID: testID(3), Slug: "acme", Name: "Acme", Status: core.TenantStatusActive, UpdatedAt: updatedAt}
	svc := &tenantNoOpAdmin{tenant: tenant}
	handler := NewPlatformHandler(svc)

	out, err := handler.UpdateTenant(context.Background(), &updateTenantInput{
		TenantID: testID(3).String(),
		IfMatch:  etagHeader(updatedAt),
		Body:     dto.TenantPatch{},
	})
	require.NoError(t, err)
	require.Zero(t, svc.updates, "an empty merge patch must not update or audit the tenant")
	require.Equal(t, etagHeader(updatedAt), out.ETag)
	require.Equal(t, "Acme", out.Body.Name)
}
