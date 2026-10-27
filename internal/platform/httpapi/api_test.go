package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type testOutput struct {
	Body struct {
		Value string `json:"value"`
	}
}

type recordingAudit struct {
	events *[]string
}

func (r recordingAudit) Record(context.Context, audit.Event) error { return nil }

func (r recordingAudit) RecordID(context.Context, audit.Event) (uuid.UUID, error) {
	*r.events = append(*r.events, "audit.intent")
	return uuid.New(), nil
}

func (r recordingAudit) Complete(context.Context, uuid.UUID, audit.Completion) error {
	*r.events = append(*r.events, "audit.complete")
	return nil
}

func marker(name string, events *[]string) fiber.Handler {
	return func(c fiber.Ctx) error {
		*events = append(*events, name)
		return c.Next()
	}
}

func testDeps(events *[]string, permissions ...authz.Permission) httpapi.Deps {
	return httpapi.Deps{
		Limiters: httpapi.Limiters{
			Strict:  marker("rate.strict", events),
			Normal:  marker("rate.normal", events),
			Relaxed: marker("rate.relaxed", events),
		},
		Authenticate: func(c fiber.Ctx) error {
			*events = append(*events, "authenticate")
			authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(uuid.New(), uuid.New(), authz.RoleAdmin, permissions...))
			return c.Next()
		},
		AuthenticateJWT: func(c fiber.Ctx) error {
			*events = append(*events, "authenticate.jwt")
			authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(uuid.New(), uuid.New(), authz.RoleAdmin, permissions...))
			return c.Next()
		},
		Idempotency: marker("idempotency", events),
		Audit:       recordingAudit{events: events},
		MFAMaxAge:   time.Hour,
	}
}

func newTestAPI(t *testing.T, deps httpapi.Deps) (*fiber.App, *httpapi.API) {
	t.Helper()
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(requestctx.WithRequestID(c.Context(), "test-request-id"))
		ctx := requestctx.WithRequestID(c.Context(), "test-request-id")
		c.SetContext(requestctx.WithPath(ctx, c.Path()))
		return c.Next()
	})
	api := httpapi.New(app, app, httpapi.DefaultConfig("Test", "1.0.0", "test"), deps)
	return app, api
}

func do(t *testing.T, app *fiber.App, req *http.Request) (resp *http.Response, body []byte) {
	t.Helper()
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestRegisterAttachesPolicyMetadataAndSecurity(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/things", OperationID: "listThings",
		Tags: []string{"things"}, Summary: "List things",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead}, func(context.Context, *struct{}) (*testOutput, error) {
		return &testOutput{}, nil
	})
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/healthz", OperationID: "getLiveness",
		Tags: []string{"platform"}, Summary: "Liveness",
		Infrastructure: true,
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*testOutput, error) {
		return &testOutput{}, nil
	})

	ops := api.Operations()
	require.Len(t, ops, 2)
	assert.False(t, ops[0].Policy.Public)
	assert.Equal(t, []map[string][]string{
		{httpapi.SecurityBearer: {}},
		{httpapi.SecurityAPIKey: {}},
	}, api.Huma().OpenAPI().Paths["/api/v1/things"].Get.Security)
	assert.Nil(t, api.Huma().OpenAPI().Paths["/healthz"].Get.Security)

	resp, _ := do(t, app, mustRequest(http.MethodGet, "/api/v1/things"))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPlatformPermissionImpliesMFA(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionPlatformTenantsRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/platform/tenants", OperationID: "listTenants",
		Tags: []string{"platform"}, Summary: "List tenants",
	}, httpapi.Policy{Permission: authz.PermissionPlatformTenantsRead}, func(context.Context, *struct{}) (*testOutput, error) {
		return &testOutput{}, nil
	})
	ops := api.Operations()
	require.Len(t, ops, 1)
	assert.True(t, ops[0].Policy.RequireMFA, "platform permissions imply MFA at registration")
	assert.True(t, ops[0].Policy.RequirePasskey, "platform permissions require a phishing-resistant passkey")

	resp, body := do(t, app, mustRequest(http.MethodGet, "/api/v1/platform/tenants"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	var problem apperror.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, string(apperror.CodeMFARequired), problem.Code)
}

func TestHandlerErrorRendersRFC9457Problem(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/missing", OperationID: "getMissing",
		Tags: []string{"test"}, Summary: "Missing",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead}, func(context.Context, *struct{}) (*testOutput, error) {
		return nil, apperror.NewNotFound("thing not found")
	})

	resp, body := do(t, app, mustRequest(http.MethodGet, "/api/v1/missing"))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/problem+json", resp.Header.Get(fiber.HeaderContentType))
	var problem apperror.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, string(apperror.CodeResourceNotFound), problem.Code)
	assert.Equal(t, "test-request-id", problem.RequestID)
	assert.Equal(t, "/api/v1/missing", problem.Instance)
}

func TestInternalErrorLogsCauseButDoesNotLeakIt(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/boom", OperationID: "getBoom",
		Tags: []string{"test"}, Summary: "Boom",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead}, func(context.Context, *struct{}) (*testOutput, error) {
		return nil, apperror.Internal(assert.AnError, "Internal Server Error")
	})

	resp, body := do(t, app, mustRequest(http.MethodGet, "/api/v1/boom"))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	var problem apperror.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, string(apperror.CodeSystemInternal), problem.Code)
	assert.NotContains(t, problem.Detail, assert.AnError.Error())
}

func TestMiddlewareOrderAndGuardPlacement(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/things", OperationID: "createThing",
		Tags: []string{"things"}, Summary: "Create thing",
	}, httpapi.Policy{
		Permission: authz.PermissionAdminUsersRead, RateLimit: httpapi.RateLimitStrict, Idempotent: true,
		Audit: httpapi.AuditPolicy{Guard: &httpapi.GuardPolicy{Action: "thing.created", Resource: "thing"}},
	}, func(context.Context, *struct{}) (*testOutput, error) {
		events = append(events, "handler")
		return &testOutput{}, nil
	})

	resp, _ := do(t, app, mustRequest(http.MethodPost, "/api/v1/things"))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{
		"rate.strict",
		"authenticate",
		"rate.strict",
		"idempotency",
		"audit.intent",
		"handler",
		"audit.complete",
	}, events)
}

func TestNoStoreAndETagHelpers(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/things/{thingId}", OperationID: "updateThing",
		Tags: []string{"things"}, Summary: "Update thing",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead, NoStore: true}, func(_ context.Context, _ *struct{}) (*testOutput, error) {
		return &testOutput{}, nil
	})
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/etag", OperationID: "getETag",
		Tags: []string{"test"}, Summary: "ETag",
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*etagOutput, error) {
		return &etagOutput{ETag: `"v1"`}, nil
	})

	resp, _ := do(t, app, mustRequest(http.MethodPatch, "/api/v1/things/abc"))
	assert.Equal(t, "no-store", resp.Header.Get(fiber.HeaderCacheControl))

	resp, _ = do(t, app, mustRequest(http.MethodGet, "/api/v1/etag"))
	assert.Equal(t, `"v1"`, resp.Header.Get(httpapi.ETagHeader))
}

type etagOutput struct {
	ETag string `header:"ETag"`
	Body struct{}
}

func mustRequest(method, path string) *http.Request {
	req, err := http.NewRequest(method, path, http.NoBody)
	if err != nil {
		panic(err)
	}
	return req
}

func TestValidatePath(t *testing.T) {
	valid := []string{
		"/api/v1/files",
		"/api/v1/files/{fileId}",
		"/api/v1/files/{fileId}/restore",
		"/api/v1/me/mfa/passkeys",
		"/api/v1/platform/tenants/{tenantId}",
	}
	for _, path := range valid {
		if err := httpapi.ValidatePath(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
	invalid := []string{
		"/api/v1/File",
		"/api/v1/files/",
		"/api/v1/files/{FileId}",
		"/api/v1/files/{id}:restore",
		"/api/v1//files",
	}
	for _, path := range invalid {
		if err := httpapi.ValidatePath(path); err == nil {
			t.Errorf("%s: expected an error", path)
		}
	}
}

func TestOutputHelpers(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodDelete, Path: "/api/v1/empty", OperationID: "deleteEmpty",
		Tags: []string{"test"}, Summary: "Delete",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead, Audit: httpapi.AuditPolicy{Tier1Action: "test.deleted"}},
		func(context.Context, *struct{}) (*httpapi.Empty, error) {
			return &httpapi.Empty{}, nil
		})
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/created", OperationID: "createCreated",
		Tags: []string{"test"}, Summary: "Create",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead, Audit: httpapi.AuditPolicy{Tier1Action: "test.created"}},
		func(context.Context, *struct{}) (*httpapi.Created[struct{}], error) {
			return httpapi.NewCreated("/api/v1/created/1", struct{}{}), nil
		})

	resp, _ := do(t, app, mustRequest(http.MethodDelete, "/api/v1/empty"))
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(fiber.HeaderContentType))

	resp, _ = do(t, app, mustRequest(http.MethodPost, "/api/v1/created"))
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, "/api/v1/created/1", resp.Header.Get("Location"))

	created := api.Huma().OpenAPI().Paths["/api/v1/created"].Post
	require.NotNil(t, created.Responses["201"], "Created must document 201")
	assert.Contains(t, created.Responses["201"].Headers, "Location")
	empty := api.Huma().OpenAPI().Paths["/api/v1/empty"].Delete
	require.NotNil(t, empty.Responses["204"], "Empty must document 204")
}

func TestRawHumaRegistrationPanics(t *testing.T) {
	var events []string
	_, api := newTestAPI(t, testDeps(&events))
	assert.Panics(t, func() {
		huma.Register(api.Huma(), huma.Operation{
			Method: http.MethodGet, Path: "/api/v1/rogue", OperationID: "rogue",
		}, func(context.Context, *struct{}) (*testOutput, error) {
			return &testOutput{}, nil
		})
	}, "a route registered outside httpapi.Register must not mount without policy")
}

func TestHumaOperationFieldNames(t *testing.T) {
	// Keep the metadata keys stable: test/arch and cmd/openapi read them.
	assert.Equal(t, "httpapi.op", httpapi.MetadataOp)
	assert.Equal(t, "httpapi.policy", httpapi.MetadataPolicy)
	var _ huma.StatusError = (*apperror.ProblemDetails)(nil)
}
