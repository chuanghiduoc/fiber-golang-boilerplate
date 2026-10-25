// Command openapi3 converts the swag-generated Swagger 2.0 spec
// (docs/swagger.json) to OpenAPI 3 so CI can run `oasdiff breaking` against
// the base branch (ADR 0006). It is a build tool only; the API never links it.
//
//	go run ./cmd/openapi3 -in docs/swagger.json -out /tmp/openapi3.json
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

func main() {
	in := flag.String("in", "docs/swagger.json", "Swagger 2.0 JSON input")
	out := flag.String("out", "", "OpenAPI 3 JSON output (required)")
	flag.Parse()
	if err := run(*in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "openapi3:", err)
		os.Exit(1)
	}
}

func run(in, out string) error {
	if out == "" {
		return errors.New("-out is required")
	}
	raw, err := os.ReadFile(in) // #nosec G304 -- operator-supplied CI path
	if err != nil {
		return fmt.Errorf("read %s: %w", in, err)
	}
	converted, err := convert(raw)
	if err != nil {
		return fmt.Errorf("convert %s: %w", in, err)
	}
	if err := os.WriteFile(out, converted, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}

// convert parses a Swagger 2.0 document and returns it as indented
// OpenAPI 3 JSON. A document that is not Swagger 2.0 is rejected, so a
// mistaken input cannot produce an empty spec that oasdiff would compare
// as "everything removed".
func convert(raw []byte) ([]byte, error) {
	var v2 openapi2.T
	if err := json.Unmarshal(raw, &v2); err != nil {
		return nil, fmt.Errorf("parse Swagger 2.0: %w", err)
	}
	if v2.Swagger != "2.0" {
		return nil, fmt.Errorf("expected swagger \"2.0\", got %q", v2.Swagger)
	}
	v3, err := openapi2conv.ToV3(&v2)
	if err != nil {
		return nil, err
	}
	// openapi2conv only emits a server when the spec has a host; swag omits
	// it, so keep basePath (/api/v1) as a relative server URL instead of
	// dropping it.
	if len(v3.Servers) == 0 && v2.BasePath != "" {
		v3.Servers = openapi3.Servers{{URL: v2.BasePath}}
	}
	return json.MarshalIndent(v3, "", "  ")
}
