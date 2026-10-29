package httpapi

import (
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func TestHumaFieldErrorsPreserveLocationsForCatalogNormalization(t *testing.T) {
	fields := humaFieldErrors([]error{
		&huma.ErrorDetail{Location: "body.name", Message: "is required"},
		&huma.ErrorDetail{Location: "query.limit", Message: "must be positive"},
	})
	if len(fields) != 2 {
		t.Fatalf("fields = %+v", fields)
	}
	if fields[0].Path != "body.name" {
		t.Errorf("body path = %q, want body.name", fields[0].Path)
	}
	if fields[1].Path != "query.limit" {
		t.Errorf("query path = %q, want query.limit", fields[1].Path)
	}
}
