package httpapi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestCheckIfMatchValue(t *testing.T) {
	cases := map[string]struct {
		ifMatch string
		wantErr bool
	}{
		"absent is unconditional":         {"", false},
		"matching value":                  {`"v2"`, false},
		"weak matching value is rejected": {`W/"v2"`, true},
		"wildcard":                        {"*", false},
		"stale value":                     {`"v1"`, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := httpapi.CheckIfMatchValue(tc.ifMatch, "v2")
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			var appErr *apperror.AppError
			if assert.ErrorAs(t, err, &appErr) {
				assert.Equal(t, string(apperror.CodeResourcePreconditionFailed), appErr.Code)
			}
		})
	}
}

func TestCheckIfMatchValueRejectsMalformedValidators(t *testing.T) {
	cases := map[string]struct {
		ifMatch string
		current string
	}{
		"empty current with wildcard":   {ifMatch: "*", current: ""},
		"empty current with quoted tag": {ifMatch: `""`, current: ""},
		"empty candidate list":          {ifMatch: ",", current: "v2"},
		"whitespace candidate list":     {ifMatch: " , ", current: "v2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := httpapi.CheckIfMatchValue(tc.ifMatch, tc.current); err == nil {
				t.Fatal("malformed If-Match must fail closed")
			}
		})
	}
}

func TestCheckIfMatchValueHandlesCommaInsideQuotedTag(t *testing.T) {
	if err := httpapi.CheckIfMatchValue(`"a,b"`, "a,b"); err != nil {
		t.Fatalf("quoted tag containing a comma was rejected: %v", err)
	}
}
