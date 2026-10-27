package config

import (
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
)

// clearTracingEnv makes default assertions deterministic even when the host
// running the tests has OTEL_* or TRACING_* variables exported.
func clearTracingEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"TRACING_ENABLED",
		"TRACING_EXCLUDE_PATHS",
		"TRACING_INSTRUMENT_DB",
		"TRACING_TRUST_INCOMING_TRACE",
		"TRACING_SHUTDOWN_TIMEOUT",
		"OTEL_ENABLED",
		"OTEL_TRACING_EXCLUDE_PATHS",
		"OTEL_INSTRUMENT_DB",
		"OTEL_TRUST_INCOMING_TRACE",
		"OTEL_SHUTDOWN_TIMEOUT_SECS",
	} {
		t.Setenv(key, "")
	}
}

func TestTracingConfigDefaultsDisabled(t *testing.T) {
	clearTracingEnv(t)

	var cfg TracingConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse tracing defaults: %v", err)
	}

	if cfg.Enabled {
		t.Error("tracing must be disabled by default")
	}
	if !cfg.InstrumentDB {
		t.Error("database instrumentation should default to true")
	}
	if cfg.TrustIncoming {
		t.Error("incoming traceparent trust must default off")
	}
	if got := cfg.ExcludedPaths(); len(got) != 4 {
		t.Errorf("ExcludedPaths() = %v, want four probe paths", got)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 10s", cfg.ShutdownTimeout)
	}
}

// TestTracingConfigUsesTRACINGPrefix pins the app-specific switch to the
// TRACING_ prefix: the removed custom OTEL_* names must be inert.
func TestTracingConfigUsesTRACINGPrefix(t *testing.T) {
	clearTracingEnv(t)
	t.Setenv("OTEL_ENABLED", "true")
	t.Setenv("OTEL_TRACING_EXCLUDE_PATHS", "/legacy")
	t.Setenv("OTEL_INSTRUMENT_DB", "false")

	var cfg TracingConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse tracing: %v", err)
	}
	if cfg.Enabled {
		t.Error("OTEL_ENABLED is not an app switch anymore; use TRACING_ENABLED")
	}
	if len(cfg.ExcludePaths) == 1 && cfg.ExcludePaths[0] == "/legacy" {
		t.Error("OTEL_TRACING_EXCLUDE_PATHS must not configure the app")
	}
	if !cfg.InstrumentDB {
		t.Error("OTEL_INSTRUMENT_DB must not configure the app")
	}

	t.Setenv("TRACING_ENABLED", "true")
	t.Setenv("TRACING_EXCLUDE_PATHS", "/healthz")
	t.Setenv("TRACING_INSTRUMENT_DB", "false")
	t.Setenv("TRACING_TRUST_INCOMING_TRACE", "true")
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse tracing overrides: %v", err)
	}
	if !cfg.Enabled || len(cfg.ExcludePaths) != 1 || cfg.ExcludePaths[0] != "/healthz" || cfg.InstrumentDB || !cfg.TrustIncoming {
		t.Errorf("TRACING_* overrides were not applied: %+v", cfg)
	}
}

func TestTracingConfig_Validate(t *testing.T) {
	valid := func() TracingConfig {
		return TracingConfig{
			Enabled:         true,
			ExcludePaths:    []string{"/healthz", "/readyz"},
			InstrumentDB:    true,
			ShutdownTimeout: 10 * time.Second,
		}
	}

	if err := valid().Validate(); err != nil {
		t.Fatalf("valid tracing config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*TracingConfig)
		want   string
	}{
		{"shutdown timeout", func(c *TracingConfig) { c.ShutdownTimeout = 0 }, "TRACING_SHUTDOWN_TIMEOUT"},
		{"exclude path", func(c *TracingConfig) { c.ExcludePaths = []string{"healthz"} }, "must start with '/'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() accepted an invalid tracing config")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestTracingConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := TracingConfig{
		Enabled:         false,
		ExcludePaths:    []string{"no-slash"},
		ShutdownTimeout: -time.Second,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled tracing must ignore invalid values, got %v", err)
	}
}

func TestConfigValidatePropagatesTracingError(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Tracing.Enabled = true
	cfg.Tracing.ShutdownTimeout = time.Second
	cfg.Tracing.ExcludePaths = []string{"healthz"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Config.Validate() must reject invalid tracing exclude paths")
	}
}

func TestTracingConfigExcludedPaths(t *testing.T) {
	cfg := TracingConfig{ExcludePaths: []string{" /healthz ", " /metrics", "", "/readyz "}}
	got := cfg.ExcludedPaths()
	if len(got) != 3 {
		t.Fatalf("ExcludedPaths() = %v, want three entries", got)
	}
}
