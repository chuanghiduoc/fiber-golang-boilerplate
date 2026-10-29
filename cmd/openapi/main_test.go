package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunWritesOpenAPI31WithoutDatabase(t *testing.T) {
	out := filepath.Join(t.TempDir(), "openapi.yaml")
	if err := run(out); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Info    map[string]any            `yaml:"info"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse YAML: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q, want 3.1.0", doc.OpenAPI)
	}
	if doc.Info["title"] == "" {
		t.Fatal("info.title is empty")
	}
	for _, path := range []string{"/healthz", "/readyz", "/health", "/metrics"} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("missing platform operation path %s", path)
		}
	}
	if !strings.Contains(string(raw), "application/problem+json") {
		t.Error("spec must document the RFC 9457 problem media type")
	}

	getMetrics, ok := doc.Paths["/metrics"]["get"].(map[string]any)
	if !ok {
		t.Fatal("metrics operation is not an object")
	}
	responses, ok := getMetrics["responses"].(map[string]any)
	if !ok {
		t.Fatal("metrics responses are not an object")
	}
	okResponse, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatal("metrics 200 response is not an object")
	}
	content, ok := okResponse["content"].(map[string]any)
	if !ok {
		t.Fatal("metrics 200 content is not an object")
	}
	if _, ok := content["text/plain; version=0.0.4; charset=utf-8"]; !ok {
		t.Error("metrics must document the Prometheus text media type")
	}
	if _, ok := content["application/json"]; ok {
		t.Error("metrics must not claim a JSON response")
	}

	for _, tc := range []struct {
		path   string
		method string
		ref    string
	}{
		{"/api/v1/me/profile", "patch", "#/components/schemas/ProfilePatch"},
		{"/api/v1/members/{memberId}", "patch", "#/components/schemas/MemberPatch"},
		{"/api/v1/roles/{roleId}", "patch", "#/components/schemas/RolePatch"},
		{"/api/v1/platform/tenants/{tenantId}", "patch", "#/components/schemas/TenantPatch"},
	} {
		path, ok := doc.Paths[tc.path]
		if !ok {
			t.Errorf("missing merge-patch path %s", tc.path)
			continue
		}
		op, ok := path[tc.method].(map[string]any)
		if !ok {
			t.Errorf("%s %s is not an object", tc.method, tc.path)
			continue
		}
		requestBody, ok := op["requestBody"].(map[string]any)
		if !ok {
			t.Errorf("%s %s has no request body", tc.method, tc.path)
			continue
		}
		content, ok := requestBody["content"].(map[string]any)
		if !ok {
			t.Errorf("%s %s has no request content", tc.method, tc.path)
			continue
		}
		media, ok := content["application/merge-patch+json"].(map[string]any)
		if !ok {
			t.Errorf("%s %s has no merge-patch content", tc.method, tc.path)
			continue
		}
		schema, ok := media["schema"].(map[string]any)
		if !ok {
			t.Errorf("%s %s has no merge-patch schema", tc.method, tc.path)
			continue
		}
		if schema["$ref"] != tc.ref {
			t.Errorf("%s %s schema = %v, want %s", tc.method, tc.path, schema["$ref"], tc.ref)
		}
	}
}

func TestGeneratedSchemaMatchesWireContracts(t *testing.T) {
	out := filepath.Join(t.TempDir(), "openapi.yaml")
	if err := run(out); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse YAML: %v", err)
	}

	for name, schema := range doc.Components.Schemas {
		if !strings.HasPrefix(name, "List") {
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", name)
		}
		data, ok := properties["data"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no data property", name)
		}
		if got := data["type"]; got != "array" {
			t.Errorf("%s data.type = %v, want array (never null)", name, got)
		}
	}

	problem, ok := doc.Components.Schemas["ProblemDetails"]
	if !ok {
		t.Fatal("missing ProblemDetails schema")
	}
	properties, ok := problem["properties"].(map[string]any)
	if !ok {
		t.Fatal("ProblemDetails has no properties")
	}
	errorsSchema, ok := properties["errors"].(map[string]any)
	if !ok {
		t.Fatal("ProblemDetails has no errors property")
	}
	if got := errorsSchema["type"]; got != "array" {
		t.Errorf("ProblemDetails errors.type = %v, want array (never null)", got)
	}
	timestamp, ok := properties["timestamp"].(map[string]any)
	if !ok {
		t.Fatal("ProblemDetails has no timestamp property")
	}
	if got := timestamp["format"]; got != "date-time" {
		t.Errorf("ProblemDetails timestamp.format = %v, want date-time", got)
	}
}
