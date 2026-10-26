package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

// emailChangeApp mounts the user routes behind a stub authenticator for an
// admin caller (id 9). viaAPIKey mirrors what apikeyhttp.Authenticate installs.
func emailChangeApp(svc *mockUserService, viaAPIKey bool) *fiber.App {
	userHandler := NewUserHandler(svc)
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	users := app.Group("/users", func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 9)
		fiber.Locals[string](c, "role", authz.RoleAdmin)
		if viaAPIKey {
			fiber.Locals[string](c, apikey.LocalAuthMethod, apikey.AuthMethodAPIKey)
			authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(9, 1, authz.RoleAdmin,
				identitycatalog.PermissionProfileWrite, authz.PermissionAdminUsersWrite).
				WithAttribute(authz.AttributeAuthMethod, apikey.AuthMethodAPIKey))
		} else {
			fiber.Locals[string](c, apikey.LocalAuthMethod, apikey.AuthMethodJWT)
			authzhttp.SetSubject(c, authz.NewSubject(9, 1, authz.RoleAdmin))
		}
		return c.Next()
	})
	users.Put("/me", userHandler.UpdateMe)
	users.Put("/:id", userHandler.Update)
	return app
}

func putUser(t *testing.T, app *fiber.App, path string, req dto.UpdateUserRequest) (status int, body string) {
	t.Helper()
	payload, err := json.Marshal(req)
	require.NoError(t, err)
	httpReq, err := http.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(httpReq)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

func ptr(s string) *string { return &s }

// Users are global identities: a tenant admin manages
// memberships, never another person's profile. Moving another account's
// address and then using forgot-password would take that account over in
// every tenant, so delegated updates are refused for JWT admins and API keys
// alike, for both email and name.
func TestUpdateOtherUser_IsAlwaysDenied(t *testing.T) {
	for name, viaAPIKey := range map[string]bool{"jwt admin": false, "api key": true} {
		t.Run(name, func(t *testing.T) {
			svc := newMockService()
			app := emailChangeApp(svc, viaAPIKey)

			status, body := putUser(t, app, "/users/1", dto.UpdateUserRequest{Email: ptr("attacker@example.com")})
			assert.Equal(t, fiber.StatusForbidden, status)
			assert.Contains(t, body, string(apperror.CodePermissionDenied))

			status, _ = putUser(t, app, "/users/1", dto.UpdateUserRequest{Name: ptr("Renamed")})
			assert.Equal(t, fiber.StatusForbidden, status)
			assert.Equal(t, "test@example.com", svc.users[1].User.Email)
			assert.Equal(t, "Test User", svc.users[1].User.Name)
		})
	}
}

// PUT /users/{caller id} behaves like PUT /users/me.
func TestUpdateSelfByID_IsAllowed(t *testing.T) {
	svc := newMockService()
	svc.users[9] = testMember(9, "self@example.com", "Self", dto.RoleAdmin)
	app := emailChangeApp(svc, false)

	status, body := putUser(t, app, "/users/9", dto.UpdateUserRequest{Name: ptr("Renamed")})
	assert.Equal(t, fiber.StatusOK, status, body)
	assert.Equal(t, "Renamed", svc.users[9].User.Name)
}

// strings.EqualFold treats U+017F (long s) as "s" but the service's
// normalization does not, so the handler must not treat this as "unchanged"
// and skip re-authentication.
func TestUpdateMeEmail_UnicodeFoldStillRequiresPassword(t *testing.T) {
	svc := newMockService()
	app := emailChangeApp(svc, false)
	// The stub authenticates as user 9, so /users/me targets this account.
	svc.users[9] = testMember(9, "test@example.com", "Self", dto.RoleAdmin)

	status, body := putUser(t, app, "/users/me", dto.UpdateUserRequest{Email: ptr("teſt@example.com")})
	assert.Equal(t, fiber.StatusBadRequest, status, body)
	assert.Contains(t, body, "currentPassword is required")
}
