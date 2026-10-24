package arch_test

import (
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// tier1Mutations are audited inside the service's database transaction. They
// must not also carry a route-level Guard intent row.
var tier1Mutations = map[string]bool{
	"POST /api/v1/auth/register":           true,
	"PUT /api/v1/users/me":                 true,
	"PUT /api/v1/users/me/password":        true,
	"PUT /api/v1/users/:id":                true,
	"DELETE /api/v1/users/:id":             true,
	"PUT /api/v1/admin/users/:id/role":     true,
	"POST /api/v1/admin/users/:id/ban":     true,
	"POST /api/v1/admin/users/:id/unban":   true,
	"DELETE /api/v1/admin/files/:id":       true,
	"POST /api/v1/admin/files/:id/restore": true,
	// API-key lifecycle: apikeys service records in the mutation transaction.
	"POST /api/v1/admin/api-keys/":           true,
	"PUT /api/v1/admin/api-keys/:id":         true,
	"DELETE /api/v1/admin/api-keys/:id":      true,
	"POST /api/v1/admin/api-keys/:id/revoke": true,
	"POST /api/v1/admin/api-keys/:id/rotate": true,
}

// fallbackAudited are existing mutating routes that are neither Tier 1 nor
// wrapped by audit.Guard. They are recorded only by the best-effort global
// audit.Middleware (plus, for the receiver, a best-effort service event), so
// an unavailable audit store does not fail them. Privileged mutations must be
// Tier 1 or guarded; only non-privileged writes belong here.
var fallbackAudited = map[string]bool{
	// Public receiver: authenticated by the source HMAC, deduplicated per
	// (source, X-Webhook-Id); every accepted event is stored.
	"POST /api/v1/webhooks/inbound/:id": true,
	// The caller's own notification state, not privileged configuration.
	"POST /api/v1/notifications/read-all":   true,
	"POST /api/v1/notifications/:id/read":   true,
	"PUT /api/v1/notification-preferences/": true,
}

func TestEveryMutatingRouteIsTier1OrGuarded(t *testing.T) {
	endpoints, _ := moduleRoutes(t)

	seenTier1 := make(map[string]bool, len(tier1Mutations))
	seenFallback := make(map[string]bool, len(fallbackAudited))
	for _, mounted := range endpoints {
		route := mounted.route
		mutating := route.Method == fiber.MethodPost ||
			route.Method == fiber.MethodPut ||
			route.Method == fiber.MethodPatch ||
			route.Method == fiber.MethodDelete ||
			(route.Method == fiber.MethodGet && strings.HasSuffix(route.Path, "/google/callback"))
		if !mutating {
			continue
		}

		key := route.Method + " " + route.Path
		if tier1Mutations[key] {
			seenTier1[key] = true
			if handlerChainContains(route, "audit.Guard") {
				t.Errorf("Tier 1 route %s must not also carry audit.Guard (double intent row)", key)
			}
			continue
		}
		if fallbackAudited[key] {
			seenFallback[key] = true
			continue
		}

		guardIndex := -1
		idempotencyIndex := -1
		for i, handler := range route.Handlers {
			name := handlerName(handler)
			if strings.Contains(name, "audit.Guard") {
				guardIndex = i
			}
			if strings.Contains(name, "testIdempotency") {
				idempotencyIndex = i
			}
		}
		if guardIndex < 0 {
			t.Errorf("module %s: mutating route %s is neither Tier 1 nor wrapped by audit.Guard", mounted.module, key)
			continue
		}
		if idempotencyIndex >= 0 && idempotencyIndex > guardIndex {
			t.Errorf("route %s must run idempotency before audit.Guard (idempotency=%d guard=%d)",
				key, idempotencyIndex, guardIndex)
		}
	}

	for key := range tier1Mutations {
		if !seenTier1[key] {
			t.Errorf("Tier 1 route %s is missing; update the coverage map with the real route", key)
		}
	}
	for key := range fallbackAudited {
		if !seenFallback[key] {
			t.Errorf("fallback-audited route %s no longer exists (or became guarded); remove it from the list", key)
		}
	}
}
