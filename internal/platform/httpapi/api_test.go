package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

type acceptedBody struct {
	ID string `json:"id"`
}

type acceptedOutput struct {
	Status int `json:"-"`
	Body   acceptedBody
}

func (*acceptedOutput) HTTPStatus() int { return http.StatusAccepted }

type recordingAudit struct {
	events *[]string
}

func (r recordingAudit) Record(context.Context, audit.Event) error { return nil }

func (r recordingAudit) RecordID(context.Context, audit.Event) (audit.IntentRef, error) {
	*r.events = append(*r.events, "audit.intent")
	return audit.IntentRef{ID: uuid.New()}, nil
}

func (r recordingAudit) Complete(context.Context, audit.IntentRef, audit.Completion) error {
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

func TestExtraSuccessStatusesAreDocumentedWithTheDefaultSchema(t *testing.T) {
	var events []string
	_, api := newTestAPI(t, testDeps(&events))

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/receipts", OperationID: "createReceipt",
		Tags: []string{"receipts"}, Summary: "Create a receipt",
		ExtraSuccessStatuses: []int{http.StatusOK},
	}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*acceptedOutput, error) {
		return &acceptedOutput{}, nil
	})

	responses := api.Huma().OpenAPI().Paths["/api/v1/receipts"].Post.Responses
	created, ok := responses["202"]
	require.True(t, ok, "the HTTPStatus default must be documented")
	duplicate, ok := responses["200"]
	require.True(t, ok, "the extra success status must be documented")
	require.NotNil(t, created.Content["application/json"])
	require.NotNil(t, duplicate.Content["application/json"])
	require.NotNil(t, duplicate.Content["application/json"].Schema)
	assert.Equal(t, created.Content["application/json"].Schema.Ref, duplicate.Content["application/json"].Schema.Ref)
	assert.NotEmpty(t, duplicate.Content["application/json"].Schema.Ref)
}

func TestExtraSuccessStatusesRejectNonSuccessStatus(t *testing.T) {
	var events []string
	_, api := newTestAPI(t, testDeps(&events))

	require.Panics(t, func() {
		httpapi.Register(api, httpapi.Op{
			Method: http.MethodPost, Path: "/api/v1/bad", OperationID: "createBad",
			Tags: []string{"bad"}, Summary: "Bad",
			ExtraSuccessStatuses: []int{http.StatusMovedPermanently},
		}, httpapi.Policy{Public: true}, func(context.Context, *struct{}) (*acceptedOutput, error) {
			return &acceptedOutput{}, nil
		})
	})
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

type strictJSONInput struct {
	Body struct {
		Name string `json:"name"`
	}
}

type rawPatchInput struct {
	ContentType string `header:"Content-Type"`
	RawBody     []byte `rawBody:"application/merge-patch+json" contentType:"application/merge-patch+json" required:"true"`
}

type rawPatchOutput struct {
	Body struct {
		ContentType string `json:"contentType"`
		Raw         string `json:"raw"`
	}
}

func TestJSONBodyRejectsUnknownAndTrailingValues(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/strict", OperationID: "createStrict",
		Tags: []string{"test"}, Summary: "Strict body",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(context.Context, *strictJSONInput) (*testOutput, error) {
			return &testOutput{}, nil
		})

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unknown member": {body: `{"name":"x","extra":1}`, want: http.StatusUnprocessableEntity},
		"trailing value": {body: `{"name":"x"} {"name":"y"}`, want: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/api/v1/strict", strings.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set(fiber.HeaderContentType, "application/json")
			resp, _ := do(t, app, req)
			assert.Equal(t, tc.want, resp.StatusCode)
		})
	}
}

func TestRawBodyMergePatchRequiresItsMediaType(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/raw-patch", OperationID: "patchRaw",
		Tags: []string{"test"}, Summary: "Raw patch",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(_ context.Context, in *rawPatchInput) (*rawPatchOutput, error) {
			out := &rawPatchOutput{}
			out.Body.ContentType = in.ContentType
			out.Body.Raw = string(in.RawBody)
			return out, nil
		})

	for _, tc := range []struct {
		name        string
		contentType string
		want        int
	}{
		{name: "missing", want: http.StatusUnsupportedMediaType},
		{name: "json", contentType: "application/json", want: http.StatusUnsupportedMediaType},
		{name: "plain", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "merge patch", contentType: "application/merge-patch+json", want: http.StatusOK},
		{name: "merge patch with charset", contentType: "application/merge-patch+json; charset=utf-8", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPatch, "/api/v1/raw-patch", strings.NewReader(`{"name":"x"}`))
			require.NoError(t, err)
			if tc.contentType != "" {
				req.Header.Set(fiber.HeaderContentType, tc.contentType)
			}
			resp, body := do(t, app, req)
			require.Equal(t, tc.want, resp.StatusCode)
			if tc.want == http.StatusOK {
				assert.Contains(t, string(body), `"contentType":"`+tc.contentType+`"`, "the handler must see the raw request Content-Type")
			}
		})
	}
}

type typedMergePatchInput struct {
	Body struct {
		Name *string `json:"name"`
	} `contentType:"application/merge-patch+json" required:"true"`
}

func TestTypedMergePatchRequiresItsMediaType(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPatch, Path: "/api/v1/typed-patch", OperationID: "patchTyped",
		Tags: []string{"test"}, Summary: "Typed patch",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(context.Context, *typedMergePatchInput) (*testOutput, error) {
			return &testOutput{}, nil
		})

	for _, tc := range []struct {
		name        string
		contentType string
		want        int
	}{
		{name: "missing", want: http.StatusUnsupportedMediaType},
		{name: "json", contentType: "application/json", want: http.StatusUnsupportedMediaType},
		{name: "plain", contentType: "text/plain", want: http.StatusUnsupportedMediaType},
		{name: "merge patch", contentType: "application/merge-patch+json", want: http.StatusOK},
		{name: "merge patch with charset", contentType: "application/merge-patch+json; charset=utf-8", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPatch, "/api/v1/typed-patch", strings.NewReader(`{"name":"x"}`))
			require.NoError(t, err)
			if tc.contentType != "" {
				req.Header.Set(fiber.HeaderContentType, tc.contentType)
			}
			resp, body := do(t, app, req)
			require.Equal(t, tc.want, resp.StatusCode, string(body))
			if tc.want == http.StatusUnsupportedMediaType {
				var problem apperror.ProblemDetails
				require.NoError(t, json.Unmarshal(body, &problem))
				assert.Equal(t, string(apperror.CodeRequestUnsupportedMediaType), problem.Code)
			}
		})
	}
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

func TestOperationBodyLimitsFollowAPI(t *testing.T) {
	var events []string
	deps := testDeps(&events)
	deps.MaxBodyBytes = 12 << 20
	deps.BodyReadTimeout = 12 * time.Second
	_, api := newTestAPI(t, deps)

	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/body-limits", OperationID: "createBodyLimitProbe",
		Tags: []string{"test"}, Summary: "Body limit probe",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(context.Context, *struct{}) (*testOutput, error) { return &testOutput{}, nil })

	op := api.Huma().OpenAPI().Paths["/api/v1/body-limits"].Post
	require.NotNil(t, op)
	assert.Equal(t, int64(12<<20), op.MaxBodyBytes)
	assert.Equal(t, 12*time.Second, op.BodyReadTimeout)
}
func TestTextPlainRequestIsRejectedWithoutPanic(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodPost, Path: "/api/v1/strict", OperationID: "createStrictPlain",
		Tags: []string{"test"}, Summary: "Strict body",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(context.Context, *strictJSONInput) (*testOutput, error) {
			return &testOutput{}, nil
		})

	req, err := http.NewRequest(http.MethodPost, "/api/v1/strict", strings.NewReader("name=x"))
	require.NoError(t, err)
	req.Header.Set(fiber.HeaderContentType, "text/plain")
	resp, body := do(t, app, req)
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	assert.NotContains(t, string(body), "panic")
}

func TestJSONEndpointRejectsUnacceptableAccept(t *testing.T) {
	var events []string
	app, api := newTestAPI(t, testDeps(&events, authz.PermissionAdminUsersRead))
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/plain-negotiation", OperationID: "getPlainNegotiation",
		Tags: []string{"test"}, Summary: "JSON only",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead},
		func(context.Context, *struct{}) (*testOutput, error) {
			return &testOutput{}, nil
		})

	req := mustRequest(http.MethodGet, "/api/v1/plain-negotiation")
	req.Header.Set(fiber.HeaderAccept, "text/plain")
	resp, body := do(t, app, req)
	require.Equal(t, http.StatusNotAcceptable, resp.StatusCode, string(body))
	var problem apperror.ProblemDetails
	require.NoError(t, json.Unmarshal(body, &problem))
	assert.Equal(t, string(apperror.CodeRequestNotAcceptable), problem.Code)

	req = mustRequest(http.MethodGet, "/api/v1/plain-negotiation")
	req.Header.Set(fiber.HeaderAccept, "application/json;q=0.9, */*;q=0.1")
	resp, _ = do(t, app, req)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get(fiber.HeaderContentType), "application/json")

	for _, tc := range []struct {
		name   string
		accept string
		want   int
	}{
		{"explicit q=0", "application/json;q=0", http.StatusNotAcceptable},
		{"all ranges q=0", "application/json;q=0, */*;q=0.0", http.StatusNotAcceptable},
		{"structured suffix outside application", "text/example+json", http.StatusNotAcceptable},
		{"application structured suffix", "application/vnd.example+json", http.StatusOK},
		{"specific q=0 overrides wildcard", "application/json;q=0, */*;q=1", http.StatusNotAcceptable},
		{"specific q=0 overrides suffix", "application/json;q=0, application/vnd.example+json;q=1", http.StatusNotAcceptable},
		{"suffix q=0 overrides application wildcard", "application/*+json;q=0, application/*;q=1", http.StatusNotAcceptable},
		{"out of range q fails closed", "application/json;q=2", http.StatusNotAcceptable},
		{"exact lower q beats wildcard", "application/json;q=0.1, */*;q=1", http.StatusOK},
		{"exact q=1 beats wildcard q=0", "application/json;q=1, */*;q=0", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mustRequest(http.MethodGet, "/api/v1/plain-negotiation")
			req.Header.Set(fiber.HeaderAccept, tc.accept)
			resp, body := do(t, app, req)
			require.Equal(t, tc.want, resp.StatusCode, string(body))
		})
	}
}

func TestDefaultConfigDoesNotShareFormatsAcrossAPIs(t *testing.T) {
	first := httpapi.DefaultConfig("First", "1.0.0", "first")
	second := httpapi.DefaultConfig("Second", "1.0.0", "second")
	delete(first.Formats, "application/json")
	_, stillPresent := second.Formats["application/json"]
	assert.True(t, stillPresent, "each API must own its format map")
}
