package arch_test

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// TestMiddlewareOrderIsEnforced pins the runtime order recorded beside every
// operation. A feature gate must run before the idempotency lock and the audit
// Guard, so a disabled operation leaves neither an intent row nor a key lock;
// authorization and MFA precede the gate so tenant/user targets apply.
func TestMiddlewareOrderIsEnforced(t *testing.T) {
	modules, deps := productionModules(t)
	application := app.New(archConfig(), deps)
	application.Register(modules...)

	for _, op := range application.HTTP().Operations() {
		key := op.Op.Method + " " + op.Op.Path
		order, ok := application.HTTP().MiddlewareOrder(op.Op.OperationID)
		if !ok {
			t.Errorf("%s has no middleware order metadata", key)
			continue
		}
		index := indexOf(order)
		if op.Policy.Feature != "" {
			feature, found := index[httpapi.MiddlewareFeature]
			if !found {
				t.Errorf("%s declares a feature gate but the chain has no feature middleware", key)
				continue
			}
			if idem, found := index[httpapi.MiddlewareIdempotency]; found && feature > idem {
				t.Errorf("%s feature gate runs after idempotency; a disabled request would take a lock", key)
			}
			if guard, found := index[httpapi.MiddlewareAuditGuard]; found && feature > guard {
				t.Errorf("%s feature gate runs after the audit Guard; a disabled request would leave an intent row", key)
			}
			if authzIndex, found := index[httpapi.MiddlewareAuthorize]; found && feature < authzIndex {
				t.Errorf("%s feature gate runs before authorization; user/tenant targets would be wrong", key)
			}
		}
		if guard, hasGuard := index[httpapi.MiddlewareAuditGuard]; hasGuard {
			if idem, hasIdem := index[httpapi.MiddlewareIdempotency]; hasIdem && guard < idem {
				t.Errorf("%s audit Guard runs before idempotency", key)
			}
			if guard != len(order)-1 {
				t.Errorf("%s audit Guard is not the final middleware before the handler", key)
			}
		}
	}
}

func indexOf(order []string) map[string]int {
	index := make(map[string]int, len(order))
	for i, label := range order {
		index[label] = i
	}
	return index
}
