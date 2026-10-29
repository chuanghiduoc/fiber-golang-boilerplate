package apikeys

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const moduleTestSecret = "api-key-module-test-secret"

var testCatalog = newTestCatalog()

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: apikeyscatalog.Owner, Grants: apikeyscatalog.Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

var (
	moduleTestAdmin  = testID(1)
	moduleTestUser   = testID(2)
	moduleTestMember = testID(3)
	moduleTestKey    = testID(5)
)

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

// fakeService records the owner/member the handler scopes each operation to.
// Route tests assert the HTTP layer passes the authenticated caller (never a
// client field) and the path member id through to the service.
type fakeService struct {
	createCalls  int
	createCaller uuid.UUID

	listOwner  uuid.UUID
	listMember uuid.UUID

	getOwner uuid.UUID

	updateOwner    uuid.UUID
	updateKey      uuid.UUID
	updateInput    apikeysvc.UpdateInput
	updateByMember uuid.UUID

	revokeOwner  uuid.UUID
	revokeMember uuid.UUID

	rotateCaller uuid.UUID
	rotateKey    uuid.UUID
}

var _ apikeysvc.Service = (*fakeService)(nil)

func sampleKey(owner uuid.UUID) *apikeysvc.APIKey {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	return &apikeysvc.APIKey{
		ID:        moduleTestKey,
		TenantID:  tenant.DefaultID,
		UserID:    owner,
		Name:      "deploy",
		Prefix:    "fgb",
		Scopes:    []authz.Permission{"files:read"},
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (s *fakeService) Create(_ context.Context, callerUserID uuid.UUID, in apikeysvc.CreateInput) (*apikeysvc.CreatedKey, error) {
	s.createCalls++
	s.createCaller = callerUserID
	key := sampleKey(callerUserID)
	key.Name = in.Name
	key.Scopes = toPermissions(in.Scopes)
	return &apikeysvc.CreatedKey{APIKey: *key, Secret: "fgb_secret"}, nil
}

func (s *fakeService) List(context.Context, int, string) ([]apikeysvc.APIKey, bool, error) {
	return []apikeysvc.APIKey{*sampleKey(moduleTestAdmin)}, false, nil
}

func (s *fakeService) ListOwnedBy(_ context.Context, ownerID uuid.UUID, _ int, _ string) ([]apikeysvc.APIKey, bool, error) {
	s.listOwner = ownerID
	return []apikeysvc.APIKey{*sampleKey(ownerID)}, false, nil
}

func (s *fakeService) ListForMember(_ context.Context, memberID uuid.UUID, _ int, _ string) ([]apikeysvc.APIKey, bool, error) {
	s.listMember = memberID
	return []apikeysvc.APIKey{*sampleKey(memberID)}, false, nil
}

func (s *fakeService) Get(_ context.Context, _ uuid.UUID) (*apikeysvc.APIKey, error) {
	return sampleKey(moduleTestAdmin), nil
}

func (s *fakeService) GetOwnedBy(_ context.Context, ownerID, _ uuid.UUID) (*apikeysvc.APIKey, error) {
	s.getOwner = ownerID
	return sampleKey(ownerID), nil
}

func (s *fakeService) Update(context.Context, uuid.UUID, apikeysvc.UpdateInput) (*apikeysvc.APIKey, error) {
	return sampleKey(moduleTestAdmin), nil
}

func (s *fakeService) UpdateOwnedBy(_ context.Context, ownerID, id uuid.UUID, in apikeysvc.UpdateInput) (*apikeysvc.APIKey, error) {
	s.updateOwner = ownerID
	s.updateKey = id
	s.updateInput = in
	key := sampleKey(ownerID)
	if in.Name != nil {
		key.Name = *in.Name
	}
	if in.Scopes != nil {
		key.Scopes = toPermissions(in.Scopes)
	}
	return key, nil
}

func (s *fakeService) UpdateForMember(_ context.Context, memberID, id uuid.UUID, in apikeysvc.UpdateInput) (*apikeysvc.APIKey, error) {
	s.updateByMember = memberID
	s.updateKey = id
	s.updateInput = in
	key := sampleKey(memberID)
	if in.Name != nil {
		key.Name = *in.Name
	}
	if in.Scopes != nil {
		key.Scopes = toPermissions(in.Scopes)
	}
	return key, nil
}

func (s *fakeService) Revoke(context.Context, uuid.UUID) error { return nil }

func (s *fakeService) RevokeOwnedBy(_ context.Context, ownerID, _ uuid.UUID) error {
	s.revokeOwner = ownerID
	return nil
}

func (s *fakeService) RevokeForMember(_ context.Context, memberID, _ uuid.UUID) error {
	s.revokeMember = memberID
	return nil
}

func (s *fakeService) Rotate(_ context.Context, callerUserID, id uuid.UUID) (*apikeysvc.RotatedKey, error) {
	s.rotateCaller = callerUserID
	s.rotateKey = id
	return &apikeysvc.RotatedKey{
		CreatedKey:            apikeysvc.CreatedKey{APIKey: *sampleKey(callerUserID), Secret: "fgb_rotated"},
		PreviousKeyID:         id,
		PreviousKeyValidUntil: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
	}, nil
}

func toPermissions(scopes []string) []authz.Permission {
	out := make([]authz.Permission, len(scopes))
	for i, scope := range scopes {
		out[i] = authz.Permission(scope)
	}
	return out
}

type moduleSessions struct{}

func (moduleSessions) Validate(_ context.Context, claims session.Claims) (session.State, error) {
	role := authz.RoleMember
	if claims.UserID == moduleTestAdmin {
		role = authz.RoleAdmin
	}
	permissions := testCatalog.PermissionsForRole(role)
	states := make([]string, 0, len(permissions))
	for permission := range permissions {
		states = append(states, string(permission))
	}
	return session.State{Role: role, Permissions: states, Active: true, TenantActive: true}, nil
}

func (moduleSessions) Invalidate(context.Context, uuid.UUID) error { return nil }

func (moduleSessions) InvalidatePermissions(context.Context) error { return nil }

func newModuleTestApp(t *testing.T, svc apikeysvc.Service) *app.App {
	t.Helper()
	cfg := &config.Config{
		App:       config.AppConfig{Env: "test", BodyLimit: 1 << 20, JSONBodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		JWT:       config.JWTConfig{Secret: moduleTestSecret, AccessTTL: time.Hour},
		CORS:      config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		Tenant:    config.TenantConfig{DefaultSlug: tenant.DefaultSlug},
	}
	application := app.New(cfg, app.Deps{
		Sessions: moduleSessions{},
	})
	application.Register(New(apikeyhandler.NewHandler(svc)))
	return application
}

func request(t *testing.T, application *app.App, method, path, role, body, contentType string) (resp *http.Response, respBody []byte) {
	t.Helper()
	userID := moduleTestAdmin
	if role != authz.RoleAdmin {
		userID = moduleTestUser
	}
	tokenValue, err := token.GenerateWithVersionAndTenant(userID, tenant.DefaultID, 0, moduleTestSecret, 1*time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	var payload io.Reader = http.NoBody
	if body != "" {
		payload = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, path, payload)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+tokenValue)
	if contentType != "" {
		req.Header.Set(fiber.HeaderContentType, contentType)
	}
	resp, err = application.Fiber().Test(req)
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

func TestModule_SelfServiceUsesAPIKeyPermissions(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	resp, body := request(t, application, http.MethodGet, "/api/v1/me/api-keys", authz.RoleMember, "", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("member list status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if svc.listOwner != moduleTestUser {
		t.Fatalf("list owner = %s, want the authenticated caller %s", svc.listOwner, moduleTestUser)
	}
	var list struct {
		Object string                     `json:"object"`
		URL    string                     `json:"url"`
		Data   []apikeydto.APIKeyResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Object != "list" || list.URL != "/api/v1/me/api-keys" {
		t.Fatalf("list envelope = %+v", list)
	}
	if len(list.Data) != 1 || list.Data[0].ID != moduleTestKey {
		t.Fatalf("list = %+v, want key %s", list.Data, moduleTestKey)
	}

	// A member cannot use the member-scoped administration surface.
	resp, body = request(t, application, http.MethodGet, "/api/v1/members/"+moduleTestMember.String()+"/api-keys", authz.RoleMember, "", "")
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member admin-list status = %d, want 403 (body=%s)", resp.StatusCode, body)
	}

	resp, body = request(t, application, http.MethodGet, "/api/v1/members/"+moduleTestMember.String()+"/api-keys", authz.RoleAdmin, "", "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("admin member-list status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if svc.listMember != moduleTestMember {
		t.Fatalf("list member = %s, want %s", svc.listMember, moduleTestMember)
	}
}

func TestModule_CreateAndRotateUseTheAuthenticatedCaller(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	// The contract has no owner field: a body that tries to set one is rejected
	// as an unknown property, and the key always belongs to the caller.
	resp, body := request(t, application, http.MethodPost, "/api/v1/me/api-keys", authz.RoleAdmin,
		`{"name":"deploy","userId":"0192c9b0-7e4a-7000-8000-000000099999","scopes":["files:read"]}`,
		fiber.MIMEApplicationJSON)
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		t.Fatalf("create with owner field status = %d, want 422 (body=%s)", resp.StatusCode, body)
	}
	if svc.createCalls != 0 {
		t.Fatal("a rejected create must not reach the service")
	}

	resp, body = request(t, application, http.MethodPost, "/api/v1/me/api-keys", authz.RoleAdmin,
		`{"name":"deploy","scopes":["files:read"]}`,
		fiber.MIMEApplicationJSON)
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", resp.StatusCode, body)
	}
	if svc.createCaller != moduleTestAdmin {
		t.Fatalf("create caller = %s, want %s", svc.createCaller, moduleTestAdmin)
	}
	if svc.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", svc.createCalls)
	}
	var created apikeydto.CreatedAPIKeyResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.UserID != moduleTestAdmin {
		t.Fatalf("created owner = %s, want the authenticated caller %s", created.UserID, moduleTestAdmin)
	}
	if created.Key != "fgb_secret" || created.ID != moduleTestKey {
		t.Fatalf("created = %+v", created)
	}
	if got := resp.Header.Get(fiber.HeaderLocation); got != "/api/v1/me/api-keys/"+moduleTestKey.String() {
		t.Fatalf("Location = %q, want /api/v1/me/api-keys/%s", got, moduleTestKey)
	}

	resp, body = request(t, application, http.MethodPost, "/api/v1/me/api-keys/"+moduleTestKey.String()+"/rotate", authz.RoleAdmin, "", "")
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("rotate status = %d, want 201 (body=%s)", resp.StatusCode, body)
	}
	if svc.rotateCaller != moduleTestAdmin || svc.rotateKey != moduleTestKey {
		t.Fatalf("rotate caller/key = %s/%s, want %s/%s", svc.rotateCaller, svc.rotateKey, moduleTestAdmin, moduleTestKey)
	}
	if got := resp.Header.Get(fiber.HeaderLocation); got != "/api/v1/me/api-keys/"+moduleTestKey.String() {
		t.Fatalf("rotate Location = %q", got)
	}
}

func TestModule_UpdateUsesMergePatch(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	resp, body := request(t, application, http.MethodPatch, "/api/v1/me/api-keys/"+moduleTestKey.String(), authz.RoleAdmin,
		`{"name":"renamed"}`, "application/merge-patch+json")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("patch status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if svc.updateOwner != moduleTestAdmin || svc.updateKey != moduleTestKey {
		t.Fatalf("update owner/key = %s/%s, want %s/%s", svc.updateOwner, svc.updateKey, moduleTestAdmin, moduleTestKey)
	}
	if svc.updateInput.Name == nil || *svc.updateInput.Name != "renamed" {
		t.Fatalf("update name = %v, want renamed", svc.updateInput.Name)
	}

	svc.updateInput = apikeysvc.UpdateInput{}
	resp, body = request(t, application, http.MethodPatch, "/api/v1/me/api-keys/"+moduleTestKey.String(), authz.RoleAdmin,
		`{"name":null}`, "application/merge-patch+json")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("null name status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
	if svc.updateInput.Name != nil || svc.updateInput.Scopes != nil {
		t.Fatal("a rejected patch must not reach the service")
	}

	resp, body = request(t, application, http.MethodPatch, "/api/v1/me/api-keys/"+moduleTestKey.String(), authz.RoleAdmin,
		`{}`, "application/merge-patch+json")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("empty patch status = %d, want 400 (body=%s)", resp.StatusCode, body)
	}
}

func TestModule_MemberRoutesAndDelete(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	resp, body := request(t, application, http.MethodPatch,
		"/api/v1/members/"+moduleTestMember.String()+"/api-keys/"+moduleTestKey.String(), authz.RoleAdmin,
		`{"scopes":["files:read"]}`, "application/merge-patch+json")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("member patch status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	if svc.updateByMember != moduleTestMember || len(svc.updateInput.Scopes) != 1 {
		t.Fatalf("member update = %s %+v", svc.updateByMember, svc.updateInput)
	}

	resp, body = request(t, application, http.MethodDelete,
		"/api/v1/members/"+moduleTestMember.String()+"/api-keys/"+moduleTestKey.String(), authz.RoleAdmin, "", "")
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("member delete status = %d, want 204 (body=%s)", resp.StatusCode, body)
	}
	if svc.revokeMember != moduleTestMember {
		t.Fatalf("revoke member = %s, want %s", svc.revokeMember, moduleTestMember)
	}

	resp, body = request(t, application, http.MethodDelete, "/api/v1/me/api-keys/"+moduleTestKey.String(), authz.RoleMember, "", "")
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("self delete status = %d, want 204 (body=%s)", resp.StatusCode, body)
	}
	if svc.revokeOwner != moduleTestUser {
		t.Fatalf("revoke owner = %s, want the authenticated caller %s", svc.revokeOwner, moduleTestUser)
	}
}

func TestModule_InvalidPathIDIsRejected(t *testing.T) {
	svc := &fakeService{}
	application := newModuleTestApp(t, svc)

	resp, body := request(t, application, http.MethodGet, "/api/v1/me/api-keys/not-a-uuid", authz.RoleAdmin, "", "")
	if resp.StatusCode != fiber.StatusUnprocessableEntity {
		t.Fatalf("malformed id status = %d, want 422 (body=%s)", resp.StatusCode, body)
	}

	resp, body = request(t, application, http.MethodGet, "/api/v1/me/api-keys/00000000-0000-0000-0000-000000000000", authz.RoleAdmin, "", "")
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("nil id status = %d, want 404 (body=%s)", resp.StatusCode, body)
	}
}

type routeExpectation struct {
	operationID string
	permission  authz.Permission
	tier        httpapi.RateLimitTier
	idempotent  bool
	noStore     bool
	tier1       string
}

func TestModule_OperationsMatchRouteMap(t *testing.T) {
	application := newModuleTestApp(t, &fakeService{})
	expected := map[string]routeExpectation{
		"GET /api/v1/me/api-keys": {
			operationID: "listMyAPIKeys", permission: "api_keys:read", tier: httpapi.RateLimitRelaxed,
		},
		"POST /api/v1/me/api-keys": {
			operationID: "createAPIKey", permission: "api_keys:create", tier: httpapi.RateLimitNormal,
			noStore: true, tier1: "api_key.created",
		},
		"GET /api/v1/me/api-keys/{keyId}": {
			operationID: "getMyAPIKey", permission: "api_keys:read", tier: httpapi.RateLimitRelaxed,
		},
		"PATCH /api/v1/me/api-keys/{keyId}": {
			operationID: "updateMyAPIKey", permission: "api_keys:update", tier: httpapi.RateLimitNormal,
			idempotent: true, tier1: "api_key.updated",
		},
		"DELETE /api/v1/me/api-keys/{keyId}": {
			operationID: "deleteMyAPIKey", permission: "api_keys:revoke", tier: httpapi.RateLimitNormal,
			idempotent: true, tier1: "api_key.revoked",
		},
		"POST /api/v1/me/api-keys/{keyId}/rotate": {
			operationID: "rotateMyAPIKey", permission: "api_keys:rotate", tier: httpapi.RateLimitNormal,
			noStore: true, tier1: "api_key.rotated",
		},
		"GET /api/v1/members/{memberId}/api-keys": {
			operationID: "listMemberAPIKeys", permission: "admin:api_keys:read", tier: httpapi.RateLimitRelaxed,
		},
		"PATCH /api/v1/members/{memberId}/api-keys/{keyId}": {
			operationID: "updateMemberAPIKey", permission: "admin:api_keys:update", tier: httpapi.RateLimitNormal,
			idempotent: true, tier1: "api_key.updated",
		},
		"DELETE /api/v1/members/{memberId}/api-keys/{keyId}": {
			operationID: "deleteMemberAPIKey", permission: "admin:api_keys:revoke", tier: httpapi.RateLimitNormal,
			idempotent: true, tier1: "api_key.revoked",
		},
	}

	seen := map[string]bool{}
	for _, op := range application.HTTP().Operations() {
		if !strings.Contains(op.Op.Path, "api-keys") {
			continue
		}
		key := op.Op.Method + " " + op.Op.Path
		want, ok := expected[key]
		if !ok {
			t.Fatalf("unexpected operation %s", key)
		}
		seen[key] = true
		if op.Op.OperationID != want.operationID {
			t.Errorf("%s operationId = %q, want %q", key, op.Op.OperationID, want.operationID)
		}
		if op.Policy.Permission != want.permission {
			t.Errorf("%s permission = %q, want %q", key, op.Policy.Permission, want.permission)
		}
		if op.Policy.RateLimit != want.tier {
			t.Errorf("%s rate tier = %q, want %q", key, op.Policy.RateLimit, want.tier)
		}
		if op.Policy.Idempotent != want.idempotent {
			t.Errorf("%s idempotent = %v, want %v", key, op.Policy.Idempotent, want.idempotent)
		}
		if op.Policy.NoStore != want.noStore {
			t.Errorf("%s no-store = %v, want %v", key, op.Policy.NoStore, want.noStore)
		}
		if op.Policy.Audit.Tier1Action != want.tier1 {
			t.Errorf("%s tier1 = %q, want %q", key, op.Policy.Audit.Tier1Action, want.tier1)
		}
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing operation %s", key)
		}
	}
	if len(expected) != len(seen) {
		t.Fatalf("registered %d operations, want %d", len(seen), len(expected))
	}
}
