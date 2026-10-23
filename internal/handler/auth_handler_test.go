package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// recordingAuditRecorder captures audit events so handler-level tests can
// assert on actor attribution through the real audit middleware.
type recordingAuditRecorder struct {
	events []audit.Event
}

func (r *recordingAuditRecorder) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

// setupAuthAuditApp mounts the auth handlers behind the real audit middleware
// so successful mutations are recorded exactly as they are in production.
func setupAuthAuditApp(recorder audit.Recorder) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
	})

	authHandler := NewAuthHandler(
		newMockService(),
		&mockRefreshTokenService{},
		&mockPasswordResetService{},
		&mockEmailVerificationService{},
		"test-secret",
		24,
		false,
		nil,
	)

	app.Use(audit.Middleware(recorder))
	app.Post("/auth/login", authHandler.Login)
	app.Post("/auth/register", authHandler.Register)
	app.Post("/auth/refresh", authHandler.Refresh)

	return app
}

func postJSON(t *testing.T, app *fiber.App, path string, payload any) int {
	t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// TestAuthAudit_ActorAttribution proves that a successful public auth mutation
// is attributed to the resolved user instead of the anonymous actor 0.
func TestAuthAudit_ActorAttribution(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		payload     any
		wantStatus  int
		wantActorID int64
	}{
		{
			name:        "login",
			path:        "/auth/login",
			payload:     dto.LoginRequest{Email: "test@example.com", Password: "Password1!"},
			wantStatus:  fiber.StatusOK,
			wantActorID: 1,
		},
		{
			name:        "register",
			path:        "/auth/register",
			payload:     dto.RegisterRequest{Email: "new@example.com", Password: "Password1!", Name: "New User"},
			wantStatus:  fiber.StatusCreated,
			wantActorID: 2,
		},
		{
			name:        "refresh",
			path:        "/auth/refresh",
			payload:     dto.RefreshRequest{RefreshToken: "valid-refresh-token"},
			wantStatus:  fiber.StatusOK,
			wantActorID: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingAuditRecorder{}
			app := setupAuthAuditApp(recorder)

			assert.Equal(t, tt.wantStatus, postJSON(t, app, tt.path, tt.payload))

			require.Len(t, recorder.events, 1)
			assert.Equal(t, tt.wantActorID, recorder.events[0].ActorID)
			assert.Equal(t, "POST "+tt.path, recorder.events[0].Action)
		})
	}
}

// TestAuthAudit_FailedLoginIsNotAttributed verifies that a failed login neither
// sets the auth local nor produces an audit event.
func TestAuthAudit_FailedLoginIsNotAttributed(t *testing.T) {
	recorder := &recordingAuditRecorder{}
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})

	authHandler := NewAuthHandler(
		newMockService(),
		&mockRefreshTokenService{},
		&mockPasswordResetService{},
		&mockEmailVerificationService{},
		"test-secret",
		24,
		false,
		nil,
	)

	var actorAfterHandler int64 = -1
	app.Use(audit.Middleware(recorder))
	app.Use(func(c fiber.Ctx) error {
		err := c.Next()
		actorAfterHandler = fiber.Locals[int64](c, "user_id")
		return err
	})
	app.Post("/auth/login", authHandler.Login)

	status := postJSON(t, app, "/auth/login", dto.LoginRequest{
		Email:    "test@example.com",
		Password: "WrongPassword2@",
	})

	assert.Equal(t, fiber.StatusUnauthorized, status)
	assert.Zero(t, actorAfterHandler)
	assert.Empty(t, recorder.events)
}
