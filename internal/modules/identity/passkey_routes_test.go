package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/google/uuid"
)

var (
	passkeyRouteUserID    = uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000201")
	passkeyRouteTenantID  = tenant.DefaultID
	passkeyRouteSessionID = uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000202")
	passkeyRouteID        = uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000203")
)

type fakePasskeyService struct {
	registrationRaw []byte
	loginRaw        []byte
	stepUpRaw       []byte
	deletedID       uuid.UUID
}

func (f *fakePasskeyService) BeginRegistration(context.Context, uuid.UUID) (*protocol.CredentialCreation, error) {
	return &protocol.CredentialCreation{
		Response: protocol.PublicKeyCredentialCreationOptions{
			Challenge: protocol.URLEncodedBase64("registration-challenge-0123456789"),
		},
	}, nil
}

func (f *fakePasskeyService) FinishRegistration(_ context.Context, userID uuid.UUID, name string, raw []byte) (*core.Passkey, error) {
	f.registrationRaw = raw
	return &core.Passkey{
		ID: passkeyRouteID, UserID: userID, Name: name,
		Transports: []string{"internal"}, CreatedAt: time.Unix(1_700_000_000, 0),
	}, nil
}

func (f *fakePasskeyService) Delete(_ context.Context, _, id uuid.UUID) error {
	f.deletedID = id
	return nil
}

func (f *fakePasskeyService) List(context.Context, uuid.UUID) ([]core.Passkey, error) {
	return nil, nil
}

func (f *fakePasskeyService) BeginLogin(context.Context, string) (*protocol.CredentialAssertion, error) {
	return &protocol.CredentialAssertion{
		Response: protocol.PublicKeyCredentialRequestOptions{
			Challenge: protocol.URLEncodedBase64("login-challenge-0123456789ab"),
		},
	}, nil
}

func (f *fakePasskeyService) FinishLogin(_ context.Context, raw []byte, _ authn.Admission) (*mfa.PasskeyLogin, error) {
	f.loginRaw = raw
	return &mfa.PasskeyLogin{
		Tokens: core.TokenPair{AccessToken: "access", RefreshToken: "refresh", SessionID: passkeyRouteSessionID},
		Member: &core.Member{
			User: core.User{ID: passkeyRouteUserID, Email: "passkey@example.com", Name: "Passkey"},
			Membership: core.Membership{
				TenantID: passkeyRouteTenantID, UserID: passkeyRouteUserID, Role: core.RoleMember,
				Status: core.MembershipStatusActive,
			},
		},
		Method: core.PasskeyMethodHardware,
	}, nil
}

func (f *fakePasskeyService) StepUpPasskey(_ context.Context, _, _ uuid.UUID, raw []byte) (time.Time, error) {
	f.stepUpRaw = raw
	return time.Unix(1_700_000_100, 0), nil
}

func newPasskeyRouteApp(t *testing.T, svc *fakePasskeyService) *fiber.App {
	t.Helper()
	fiberApp := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	auth := func(c fiber.Ctx) error {
		authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(
			passkeyRouteUserID, passkeyRouteTenantID, authz.RoleMember, identitycatalog.PermissionMFAWrite))
		c.SetContext(requestctx.WithSessionID(c.Context(), passkeyRouteSessionID))
		return c.Next()
	}
	api := httpapi.New(fiberApp, fiberApp, httpapi.DefaultConfig("Test", "1.0.0", "test"), httpapi.Deps{
		Authenticate:    auth,
		AuthenticateJWT: auth,
	})
	module := &Module{passkeys: identityhandler.NewPasskeyHandler(svc, nil, nil)}
	module.registerPasskeyRoutes(api, app.Deps{})
	return fiberApp
}

func passkeyRouteRequest(t *testing.T, application *fiber.App, method, path, body string) (status int, response []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test")
	resp, err := application.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func TestPasskeyRoutes_RegistrationEnvelopeAndDirectBody(t *testing.T) {
	svc := &fakePasskeyService{}
	application := newPasskeyRouteApp(t, svc)

	status, body := passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/me/mfa/passkeys/options", "")
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), "challenge")

	credential := `"id":"AQ","rawId":"AQ","type":"public-key","response":{"clientDataJSON":"e30","attestationObject":"e30"}`
	envelope := fmt.Sprintf(`{"name":"Laptop","credential":{%s}}`, credential)
	status, body = passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/me/mfa/passkeys", envelope)
	require.Equal(t, http.StatusCreated, status, string(body))
	var response struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	assert.Equal(t, passkeyRouteID, response.ID)
	assert.Equal(t, "Laptop", response.Name)
	assert.Contains(t, string(svc.registrationRaw), `"rawId":"AQ"`)
	assert.NotContains(t, string(svc.registrationRaw), `"name"`)

	// The browser's bare PublicKeyCredential shape is accepted too.
	status, body = passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/me/mfa/passkeys", "{"+credential+"}")
	require.Equal(t, http.StatusCreated, status, string(body))
	assert.Contains(t, string(svc.registrationRaw), `"rawId":"AQ"`)
}

func TestPasskeyRoutes_LoginAndStepUp(t *testing.T) {
	svc := &fakePasskeyService{}
	application := newPasskeyRouteApp(t, svc)

	status, body := passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/auth/passkeys/options", `{}`)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), "challenge")

	credential := `"id":"AQ","rawId":"AQ","type":"public-key","response":{"clientDataJSON":"e30","authenticatorData":"e30","signature":"e30"}`
	status, body = passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/auth/passkeys/verifications",
		`{"credential":{`+credential+`}}`)
	require.Equal(t, http.StatusOK, status, string(body))
	var login struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	require.NoError(t, json.Unmarshal(body, &login))
	assert.Equal(t, "access", login.AccessToken)
	assert.Equal(t, "refresh", login.RefreshToken)

	status, body = passkeyRouteRequest(t, application, http.MethodPost, "/api/v1/me/mfa/step-up",
		`{"passkey":{`+credential+`}}`)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), "verifiedAt")
	assert.Contains(t, string(svc.stepUpRaw), `"signature":"e30"`)
}

func TestPasskeyRoutes_DeleteUsesPathID(t *testing.T) {
	svc := &fakePasskeyService{}
	application := newPasskeyRouteApp(t, svc)

	status, body := passkeyRouteRequest(t, application, http.MethodDelete,
		"/api/v1/me/mfa/passkeys/"+passkeyRouteID.String(), "")
	require.Equal(t, http.StatusNoContent, status, string(body))
	assert.Equal(t, passkeyRouteID, svc.deletedID)
}

func TestPasskeyRoutes_RequireMFAWritePermission(t *testing.T) {
	svc := &fakePasskeyService{}
	fiberApp := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	auth := func(c fiber.Ctx) error {
		authzhttp.SetSubject(c, authz.NewSubjectWithPermissions(
			passkeyRouteUserID, passkeyRouteTenantID, authz.RoleMember, "profile:read"))
		return c.Next()
	}
	api := httpapi.New(fiberApp, fiberApp, httpapi.DefaultConfig("Test", "1.0.0", "test"), httpapi.Deps{
		Authenticate: auth, AuthenticateJWT: auth,
	})
	module := &Module{passkeys: identityhandler.NewPasskeyHandler(svc, nil, nil)}
	module.registerPasskeyRoutes(api, app.Deps{})

	status, body := passkeyRouteRequest(t, fiberApp, http.MethodPost, "/api/v1/me/mfa/passkeys/options", "")
	assert.Equal(t, http.StatusForbidden, status, string(body))
	assert.Contains(t, string(body), string(apperror.CodePermissionDenied))
	assert.Nil(t, svc.registrationRaw)
}
