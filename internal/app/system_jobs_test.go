package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

type handlerRegistrar map[string]jobs.Handler

func (r handlerRegistrar) Register(kind string, handler jobs.Handler) { r[kind] = handler }

func TestSystemRegistrarMarksJobHandlersAsSystemCallers(t *testing.T) {
	inner := handlerRegistrar{}
	var sawSystem bool
	systemRegistrar{next: inner}.Register("test.kind", func(ctx context.Context, _ json.RawMessage) error {
		sawSystem = authz.IsSystemContext(ctx)
		return nil
	})

	handler, ok := inner["test.kind"]
	if !ok {
		t.Fatal("handler was not registered with the wrapped registrar")
	}
	if err := handler(context.Background(), nil); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if !sawSystem {
		t.Fatal("job handler context must be marked with authz.SystemContext")
	}
}
