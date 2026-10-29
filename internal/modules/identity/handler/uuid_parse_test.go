package handler

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestParseUUIDNilIsNotFound(t *testing.T) {
	_, err := parseUUID(uuid.Nil.String(), "sessionId")

	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(apperror.CodeResourceNotFound), appErr.Code)
}

func TestParseUUIDMalformedIsValidation(t *testing.T) {
	_, err := parseUUID("not-a-uuid", "sessionId")

	var appErr *apperror.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, string(apperror.CodeRequestValidationFailed), appErr.Code)
	require.Len(t, appErr.Errors, 1)
	assert.Equal(t, "path.sessionId", appErr.Errors[0].Path)
}
