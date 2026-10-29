package handler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

type mfaStatusFake struct {
	MFA
	enrollment *core.MFAEnrollment
	factor     *core.MFAFactorStatus
}

func (f *mfaStatusFake) EnrollTOTP(context.Context, uuid.UUID, core.ReauthProof) (*core.MFAEnrollment, error) {
	return f.enrollment, nil
}

func (f *mfaStatusFake) GetTOTP(_ context.Context, _, factorID uuid.UUID) (*core.MFAFactorStatus, error) {
	if f.factor == nil || f.factor.ID != factorID {
		return nil, apperror.NewNotFound("MFA factor not found")
	}
	return f.factor, nil
}

func mfaTestContext(userID uuid.UUID) context.Context {
	return authz.WithSubject(context.Background(), authz.NewSubject(userID, testID(1), authz.RoleMember))
}

func TestMFAHandler_EnrollLocationIsDereferenceableStatus(t *testing.T) {
	userID := testID(20)
	factorID := testID(21)
	createdAt := time.Unix(1_700_000_000, 0)
	confirmedAt := createdAt.Add(time.Minute)
	fake := &mfaStatusFake{
		enrollment: &core.MFAEnrollment{FactorID: factorID, OtpauthURI: "otpauth://totp/test?secret=AAAA"},
		factor: &core.MFAFactorStatus{
			ID: factorID, Type: core.MFAFactorTOTP, Status: core.MFAFactorActive,
			CreatedAt: createdAt, ConfirmedAt: &confirmedAt,
		},
	}
	handler := NewMFAHandler(fake)
	ctx := mfaTestContext(userID)

	created, err := handler.Enroll(ctx, &mfaEnrollInput{})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/me/mfa/totp/"+factorID.String(), created.Location)
	assert.NotEmpty(t, created.Body.OtpauthURI)

	status, err := handler.GetTOTP(ctx, &mfaFactorInput{FactorID: factorID.String()})
	require.NoError(t, err)
	assert.Equal(t, factorID, status.Body.FactorID)
	assert.Equal(t, core.MFAFactorActive, status.Body.Status)
	assert.NotContains(t, status.Body.Type, "secret")
	assert.Equal(t, &confirmedAt, status.Body.ConfirmedAt)

	_, err = handler.GetTOTP(ctx, &mfaFactorInput{FactorID: testID(99).String()})
	require.Error(t, err)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}
