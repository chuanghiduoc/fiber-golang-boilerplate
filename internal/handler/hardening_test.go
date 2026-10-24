package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
)

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
