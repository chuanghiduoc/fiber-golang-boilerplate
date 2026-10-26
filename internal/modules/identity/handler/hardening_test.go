package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func TestGetByID_OtherUserForbidden(t *testing.T) {
	svc := newMockService()
	svc.users[2] = testMember(2, "other@example.com", "Other", "user")
	app := setupApp(svc)

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)
	req, _ := http.NewRequest("GET", "/users/2", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUpdateMe_EmailChangeRequiresPassword(t *testing.T) {
	app := setupApp(newMockService())
	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)

	body, _ := json.Marshal(map[string]string{"email": "new@example.com"})
	req, _ := http.NewRequest("PUT", "/users/me", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	body, _ = json.Marshal(map[string]string{
		"email":           "new@example.com",
		"currentPassword": "Password1!2026",
	})
	req, _ = http.NewRequest("PUT", "/users/me", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestRegisterHandler_RejectsUnknownFields(t *testing.T) {
	app := setupApp(newMockService())

	body := []byte(`{"email":"new@example.com","password":"Password1!2026","name":"New","isAdmin":true}`)
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestLogoutAll_RequiresAuth(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/logout-all", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)
	req, _ = http.NewRequest("POST", "/auth/logout-all", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestUserList_ReturnsNextCursor(t *testing.T) {
	svc := newMockService()
	svc.hasMore = true
	svc.users[1].User.CreatedAt = time.Now()
	app := setupApp(svc)

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "test-secret", 24)
	req, _ := http.NewRequest("GET", "/users/", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var list struct {
		HasMore    bool   `json:"hasMore"`
		NextCursor string `json:"nextCursor"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	require.True(t, list.HasMore)
	require.NotEmpty(t, list.NextCursor)

	ts, id, err := pagination.DecodeCursor(list.NextCursor)
	require.NoError(t, err)
	require.Equal(t, int64(1), id)
	require.False(t, ts.IsZero())
}
