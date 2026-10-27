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
}
