package apikeys

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const moduleTestSecret = "api-key-module-test-secret"

type fakeService struct {
	createCalls int
	created     *apikeysvc.CreatedKey
}

var _ apikeysvc.Service = (*fakeService)(nil)

func (s *fakeService) Create(_ context.Context, callerUserID int64, in apikeysvc.CreateInput) (*apikeysvc.CreatedKey, error) {
	s.createCalls++
	if s.created != nil {
		return s.created, nil
	}
	return &apikeysvc.CreatedKey{
		APIKey: apikeysvc.APIKey{
			ID:        5,
			UserID:    callerUserID,
			Name:      in.Name,
			Prefix:    "fgb",
			Scopes:    toPermissions(in.Scopes),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
		Secret: "fgb_secret",
	}, nil
}

func (s *fakeService) List(context.Context, int, string) ([]apikeysvc.APIKey, bool, error) {
	return []apikeysvc.APIKey{{ID: 5, Name: "deploy", Scopes: toPermissions([]string{"files:read"})}}, false, nil
}

func (s *fakeService) Get(context.Context, int64) (*apikeysvc.APIKey, error) {
	return &apikeysvc.APIKey{ID: 5, Name: "deploy"}, nil
}

func (s *fakeService) Update(context.Context, int64, apikeysvc.UpdateInput) (*apikeysvc.APIKey, error) {
	return &apikeysvc.APIKey{ID: 5, Name: "deploy"}, nil
}

func (s *fakeService) Revoke(context.Context, int64) error { return nil }

func (s *fakeService) Rotate(context.Context, int64) (*apikeysvc.RotatedKey, error) {
	return &apikeysvc.RotatedKey{}, nil
}

func toPermissions(scopes []string) []authz.Permission {
	out := make([]authz.Permission, len(scopes))
	for i, scope := range scopes {
		out[i] = authz.Permission(scope)
	}
	return out
}

func newModuleTestApp(t *testing.T, svc apikeysvc.Service) *fiber.App {
	t.Helper()
	application := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	pass := func(c fiber.Ctx) error { return c.Next() }
	deps := app.Deps{
		Config:      &config.Config{JWT: config.JWTConfig{Secret: moduleTestSecret}},
		Limiters:    app.Limiters{Normal: pass},
		Idempotency: pass,
	}
	v1 := application.Group("/api/v1")
	New(apikeyhandler.NewHandler(svc)).RegisterRoutes(v1, deps)
	return application
}

func request(t *testing.T, application *fiber.App, method, path, role, body string) (resp *http.Response, respBody []byte) {
	t.Helper()
	tokenValue, err := token.GenerateWithVersionAndTenant(1, 1, "admin@test.com", role, 0, moduleTestSecret, 1)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	req, err := http.NewRequest(method, path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+tokenValue)
	if body != "" {
		req.Header.Set(fiber.HeaderContentType, "application/json")
	}
	resp, err = application.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, respBody
}

func TestModule_RoutePermissions(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	body := `{"name":"deploy","scopes":["files:read"]}`
	resp, respBody := request(t, application, http.MethodPost, "/api/v1/admin/api-keys/", authz.RoleMember, body)
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("user create status = %d, want 403 (body=%s)", resp.StatusCode, respBody)
	}
	if svc.createCalls != 0 {
		t.Fatal("service must not run without admin:api_keys:create")
	}

	resp, respBody = request(t, application, http.MethodPost, "/api/v1/admin/api-keys/", authz.RoleAdmin, body)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("admin create status = %d, want 201 (body=%s)", resp.StatusCode, respBody)
	}
	if got := resp.Header.Get(fiber.HeaderLocation); got != "/api/v1/admin/api-keys/5" {
		t.Fatalf("Location = %q, want /api/v1/admin/api-keys/5", got)
	}
	if svc.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", svc.createCalls)
	}
	var created apikeydto.KeyWithSecretResponse
	if err := json.Unmarshal(respBody, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Key != "fgb_secret" || created.ID != 5 {
		t.Fatalf("created = %+v", created)
	}

	resp, respBody = request(t, application, http.MethodGet, "/api/v1/admin/api-keys/", authz.RoleAdmin, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200 (body=%s)", resp.StatusCode, respBody)
	}
	var list struct {
		Data []apikeydto.APIKeyResponse `json:"data"`
	}
	if err := json.Unmarshal(respBody, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != 5 {
		t.Fatalf("list = %+v, want key 5", list.Data)
	}

	resp, respBody = request(t, application, http.MethodDelete, "/api/v1/admin/api-keys/5", authz.RoleAdmin, "")
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204 (body=%s)", resp.StatusCode, respBody)
	}
}

func TestModule_RoutesWithoutTrailingSlash(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	resp, body := request(t, application, http.MethodGet, "/api/v1/admin/api-keys", authz.RoleAdmin, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
}
