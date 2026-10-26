package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/validator"
)

// The change-password endpoint knows the caller's address and refuses a new
// password built from it. The tag validator only sees the password field, so
// the handler applies the service policy after loading the member.
func TestChangePasswordHandler_RejectsPasswordContainingEmail(t *testing.T) {
	app := setupApp(newMockService())

	body, _ := json.Marshal(dto.ChangePasswordRequest{
		CurrentPassword: "OldPassword1!2026",
		NewPassword:     "test-my-secret-passphrase",
	})
	req, _ := http.NewRequest("PUT", "/users/me/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)

	var problem apperror.ProblemDetails
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&problem))
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, "newPassword", problem.Errors[0].Path)
	assert.Equal(t, validator.CodePasswordFromEmail, problem.Errors[0].Code)
}

// The reset endpoint has only the token; the reset service resolves the
// address a live token belongs to, and the handler applies the same policy
// before the password is stored.
func TestResetPasswordHandler_RejectsPasswordContainingEmail(t *testing.T) {
	resets := &mockPasswordResetService{email: "alice@example.com"}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	authHandler := newTestAuthHandler(
		newMockService(), &mockSessions{}, resets, &mockEmailVerificationService{}, "test-secret", 24, false, nil,
	)
	app.Post("/auth/reset-password", authHandler.ResetPassword)

	body, _ := json.Marshal(dto.ResetPasswordRequest{
		Token:    "reset-token",
		Password: "alice-in-wonderland-passphrase",
	})
	req, _ := http.NewRequest("POST", "/auth/reset-password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, fiber.StatusUnprocessableEntity, resp.StatusCode)

	var problem apperror.ProblemDetails
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&problem))
	require.Len(t, problem.Errors, 1)
	assert.Equal(t, "password", problem.Errors[0].Path)
	assert.Equal(t, validator.CodePasswordFromEmail, problem.Errors[0].Code)
}
