package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func TestGetByID_OtherUserForbidden(t *testing.T) {
	svc := newMockService()
	svc.users[2] = &dto.UserResponse{ID: 2, Email: "other@example.com", Name: "Other", Role: "user"}
	app := setupApp(svc)

	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)
	req, _ := http.NewRequest("GET", "/users/2", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)
}

func TestUpdateMe_EmailChangeRequiresPassword(t *testing.T) {
	app := setupApp(newMockService())
	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)

	body, _ := json.Marshal(map[string]string{"email": "new@example.com"})
	req, _ := http.NewRequest("PUT", "/users/me", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	body, _ = json.Marshal(map[string]string{
		"email":           "new@example.com",
		"currentPassword": "Password1!",
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

	body := []byte(`{"email":"new@example.com","password":"Password1!","name":"New","isAdmin":true}`)
	req, _ := http.NewRequest("POST", "/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestUploadHandler_RejectsOversizedFile(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	uploadHandler := NewUploadHandler(newMockUploadService(), 10, []string{"text/plain; charset=utf-8"})
	app.Post("/upload", middleware.JWTAuth("test-secret"), uploadHandler.Upload)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "large.txt")
	_, _ = part.Write(bytes.Repeat([]byte("x"), 32))
	_ = writer.Close()

	req, _ := http.NewRequest("POST", "/upload", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+userToken())

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestLogoutAll_RequiresAuth(t *testing.T) {
	app := setupApp(newMockService())

	req, _ := http.NewRequest("POST", "/auth/logout-all", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusUnauthorized, resp.StatusCode)

	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)
	req, _ = http.NewRequest("POST", "/auth/logout-all", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusNoContent, resp.StatusCode)
}

func TestAdmin_SelfRoleAndBanGuards(t *testing.T) {
	app := setupAdminApp()
	tok := adminToken()

	body, _ := json.Marshal(dto.UpdateRoleRequest{Role: "user"})
	req, _ := http.NewRequest("PUT", "/admin/users/99/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)

	req, _ = http.NewRequest("POST", "/admin/users/99/ban", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusBadRequest, resp.StatusCode)
}

func TestUserList_ReturnsNextCursor(t *testing.T) {
	svc := newMockService()
	svc.hasMore = true
	svc.users[1].CreatedAt = time.Now()
	app := setupApp(svc)

	accessToken, _ := token.Generate(1, "test@example.com", "user", "test-secret", 24)
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

func TestAdminFileRoutes(t *testing.T) {
	app := setupAdminApp()
	tok := adminToken()

	req, _ := http.NewRequest("GET", "/admin/files/1/download", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)

	req, _ = http.NewRequest("POST", "/admin/files/1/restore", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, fiber.StatusOK, resp.StatusCode)
}
