package handler

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type memberErasureFake struct {
	PrivacyService
	request *core.ErasureRequest
}

func (f *memberErasureFake) RequestAdminErasure(context.Context, uuid.UUID) error { return nil }

func (f *memberErasureFake) GetMemberErasureRequest(_ context.Context, userID, requestID uuid.UUID) (*core.ErasureRequest, error) {
	if requestID != userID {
		return nil, apperror.NewNotFound("erasure request not found")
	}
	return f.request, nil
}

func TestPrivacyHandler_AdminErasureLocationIsDereferenceable(t *testing.T) {
	tenantID := testID(1)
	memberID := testID(30)
	requestedAt := time.Unix(1_700_000_000, 0)
	fake := &memberErasureFake{request: &core.ErasureRequest{
		ID: memberID, TenantID: tenantID, UserID: memberID,
		Scope: core.ErasureScopeMember, Status: core.ErasureProcessing, RequestedAt: requestedAt,
	}}
	handler := NewPrivacyHandler(fake)
	ctx := tenant.WithID(context.Background(), tenantID)

	created, err := handler.EraseUser(ctx, &memberErasureInput{MemberID: memberID.String()})
	require.NoError(t, err)
	location := "/api/v1/members/" + memberID.String() + "/erasure-requests/" + memberID.String()
	assert.Equal(t, location, created.Location)

	status, err := handler.GetMemberErasureRequest(ctx, &memberErasureRequestInput{
		MemberID: memberID.String(), RequestID: memberID.String(),
	})
	require.NoError(t, err)
	assert.Equal(t, memberID, status.Body.ID)
	assert.Equal(t, "member", status.Body.Scope)
	assert.Equal(t, "processing", status.Body.Status)

	_, err = handler.GetMemberErasureRequest(ctx, &memberErasureRequestInput{
		MemberID: memberID.String(), RequestID: testID(99).String(),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}
