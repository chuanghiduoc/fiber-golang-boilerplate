package app

import (
	"context"
	"encoding/json"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

// systemRegistrar runs every module and durable-event job handler with
// authz.SystemContext. Job handlers (scheduled work included, since the
// scheduler only enqueues jobs) act for the platform, not for a user, so they
// carry no subject; without the marker authz.Authorize would deny them.
type systemRegistrar struct {
	next jobs.Registrar
}

func (r systemRegistrar) Register(kind string, handler jobs.Handler) {
	r.next.Register(kind, func(ctx context.Context, payload json.RawMessage) error {
		return handler(authz.SystemContext(ctx), payload)
	})
}
