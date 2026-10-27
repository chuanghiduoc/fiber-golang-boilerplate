package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humafiber"
	"github.com/gofiber/fiber/v3"
)

// fiberAdapter mounts huma operations on Fiber and, unlike humafiber's adapter,
// prepends the policy's Fiber middleware chain. Fiber's own router then runs
// the chain in order, so existing middleware (JWT/API-key auth, authz, MFA,
// idempotency, audit guard, limiters) works unchanged.
type fiberAdapter struct {
	app    *fiber.App
	router fiber.Router
}

var _ huma.Adapter = (*fiberAdapter)(nil)

func (a *fiberAdapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	if _, ok := op.Metadata[MetadataOp]; !ok && !metaPath(op.Path) {
		panic(fmt.Sprintf("httpapi: operation %s %s bypassed httpapi.Register", op.Method, op.Path))
	}
	chain, _ := op.Metadata[metadataMiddlewares].([]fiber.Handler)
	final := func(c fiber.Ctx) error {
		handler(humafiber.NewContext(op, c))
		return nil
	}
	handlers := make([]any, 0, len(chain)+1)
	for _, middleware := range chain {
		handlers = append(handlers, middleware)
	}
	handlers = append(handlers, final)
	a.router.Add([]string{op.Method}, toFiberPath(op.Path), handlers[0], handlers[1:]...)
}

// ServeHTTP adapts the Fiber app to net/http for huma internals and tests.
func (a *fiberAdapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resp, err := a.app.Test(r)
	if err != nil {
		panic(err)
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// toFiberPath converts huma's {param} syntax to Fiber's :param syntax.
func toFiberPath(path string) string {
	if !strings.ContainsAny(path, "{}") {
		return path
	}
	path = strings.ReplaceAll(path, "{", ":")
	return strings.ReplaceAll(path, "}", "")
}

// metaPath identifies huma's own spec/schema endpoints, which are registered
// before any module operation and carry no policy metadata.
func metaPath(path string) bool {
	return strings.HasPrefix(path, "/openapi") || strings.HasPrefix(path, "/schemas/")
}
