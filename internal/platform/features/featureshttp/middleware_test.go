package featureshttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

// keyTestUploads is a stand-in, default-on flag registered for this package's
// tests so they stay independent of any business module.
const keyTestUploads features.Key = "featureshttp_test_uploads"

var testCatalog = newTestCatalog()

func newTestCatalog() *features.Catalog {
	catalog := features.NewCatalog()
	if err := catalog.Add(features.Provider{Owner: "featureshttptest", Definitions: []features.Definition{
		{Key: keyTestUploads, Description: "test", Default: true},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func TestMiddlewareEvaluatesRequestTenantAndUser(t *testing.T) {
	snapshot := features.Snapshot{
		UserTargets: map[features.Key]map[features.UserTargetKey]bool{
			keyTestUploads: {{TenantID: testID(7), UserID: testID(9)}: false},
		},
	}
	store := features.StoreFunc(func(context.Context) (features.Snapshot, error) { return snapshot, nil })
	flags := features.NewEvaluator(features.Options{Catalog: testCatalog, Store: store})

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), testID(7)))
		if c.Get("X-User") == "9" {
			c.SetContext(requestctx.WithActorID(c.Context(), testID(9)))
		}
		return c.Next()
	})
	app.Post("/upload", Middleware(flags, keyTestUploads), func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusCreated)
	})

	req := httptest.NewRequest(http.MethodPost, "/upload", http.NoBody)
	req.Header.Set("X-User", "9")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("targeted user status = %d, want 404", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(http.MethodPost, "/upload", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("other user status = %d, want 201", resp.StatusCode)
	}
}
