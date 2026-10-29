package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
)

type validationInput struct {
	Limit int `query:"limit" minimum:"1" required:"true"`
}

func registerValidationRoute(t *testing.T, api *httpapi.API, operationID, path string) {
	t.Helper()
	httpapi.Register(api, httpapi.Op{
		Method:         http.MethodGet,
		Path:           path,
		OperationID:    operationID,
		Tags:           []string{"test"},
		Summary:        "Validate query",
		Infrastructure: true,
	}, httpapi.Policy{Public: true}, func(context.Context, *validationInput) (*testOutput, error) {
		return &testOutput{}, nil
	})
}

func validationMessage(t *testing.T, app *fiber.App, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	require.NoError(t, err)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Errors []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.Errors)
	return body.Errors[0].Message
}

// TestHumaErrorHooksUsePerAPIRenderer builds A, then B with a different i18n
// bundle, and verifies A's validation error still uses A's renderer. Before the
// per-request hook, constructing B replaced the package-global constructor.
func TestHumaErrorHooksUsePerAPIRenderer(t *testing.T) {
	var eventsA, eventsB []string
	depsA := testDeps(&eventsA)
	depsA.I18n = nil
	appA, apiA := newTestAPI(t, depsA)
	registerValidationRoute(t, apiA, "validateA", "/validation-a")

	baseline := validationMessage(t, appA, "/validation-a")

	bundle, err := i18n.New("en")
	require.NoError(t, err)
	depsB := testDeps(&eventsB)
	depsB.I18n = bundle
	appB, apiB := newTestAPI(t, depsB)
	registerValidationRoute(t, apiB, "validateB", "/validation-b")

	assert.Equal(t, baseline, validationMessage(t, appA, "/validation-a"),
		"constructing API B must not change API A's error renderer")
	assert.NotEqual(t, baseline, validationMessage(t, appB, "/validation-b"),
		"API B should localize validation messages with its own bundle")
}
