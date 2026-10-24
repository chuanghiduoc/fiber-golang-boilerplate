package config

import (
	"strings"
	"testing"

	"github.com/caarlos0/env/v11"
)

// clearTracingEnv makes default assertions deterministic even when the host
// running the tests has OTEL_* variables exported.
func clearTracingEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_ENABLED",
		"OTEL_TRACES_EXPORTER",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_SERVICE_NAME",
		"OTEL_SERVICE_NAMESPACE",
		"OTEL_SERVICE_VERSION",
		"OTEL_DEPLOYMENT_ENVIRONMENT",
		"OTEL_SERVICE_INSTANCE_ID",
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
		"OTEL_TRACING_EXCLUDE_PATHS",
		"OTEL_INSTRUMENT_DB",
		"OTEL_BATCH_TIMEOUT_SECS",
		"OTEL_EXPORT_TIMEOUT_SECS",
		"OTEL_SHUTDOWN_TIMEOUT_SECS",
		"OTEL_BATCH_MAX_QUEUE_SIZE",
		"OTEL_BATCH_MAX_EXPORT_BATCH_SIZE",
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
	if cfg.Exporter != "otlp" {
		t.Errorf("Exporter = %q, want otlp", cfg.Exporter)
	}
	if cfg.Protocol != "grpc" {
		t.Errorf("Protocol = %q, want grpc", cfg.Protocol)
	}
	if cfg.Endpoint != "localhost:4317" {
		t.Errorf("Endpoint = %q, want localhost:4317", cfg.Endpoint)
	}
	if !cfg.Insecure {
		t.Error("Insecure should default to true for local development")
	}
	if cfg.Sampler != "parentbased_traceidratio" {
		t.Errorf("Sampler = %q, want parentbased_traceidratio", cfg.Sampler)
	}
	if cfg.SamplerArg != 1 {
		t.Errorf("SamplerArg = %v, want 1", cfg.SamplerArg)
	}
	if cfg.ServiceName == "" || cfg.DeploymentEnvironment == "" {
		t.Error("service name and deployment environment must have defaults")
	}
	if !cfg.InstrumentDB {
		t.Error("database instrumentation should default to true")
	}
	if got := cfg.ExcludedPaths(); len(got) != 4 {
		t.Errorf("ExcludedPaths() = %v, want four probe paths", got)
	}
	if cfg.ShutdownTimeout < 1 || cfg.MaxQueueSize < 1 || cfg.MaxExportBatchSize < 1 {
		t.Error("batch and shutdown defaults must be positive")
	}
}

func TestTracingConfig_Validate(t *testing.T) {
	valid := func() TracingConfig {
		return TracingConfig{
			Enabled:               true,
			Exporter:              "otlp",
			Protocol:              "grpc",
			Endpoint:              "collector:4317",
			Insecure:              true,
			ServiceName:           "api",
			DeploymentEnvironment: "staging",
			Sampler:               "parentbased_traceidratio",
			SamplerArg:            0.25,
			ExcludePaths:          "/healthz, /readyz",
			BatchTimeout:          5,
			ExportTimeout:         30,
			ShutdownTimeout:       10,
			MaxQueueSize:          2048,
			MaxExportBatchSize:    512,
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
		{"exporter", func(c *TracingConfig) { c.Exporter = "http" }, "OTEL_TRACES_EXPORTER"},
		{"protocol", func(c *TracingConfig) { c.Protocol = "http" }, "OTEL_EXPORTER_OTLP_PROTOCOL"},
		{"sampler", func(c *TracingConfig) { c.Sampler = "bogus" }, "OTEL_TRACES_SAMPLER"},
		{"sampler arg low", func(c *TracingConfig) { c.SamplerArg = -0.1 }, "OTEL_TRACES_SAMPLER_ARG"},
		{"sampler arg high", func(c *TracingConfig) { c.SamplerArg = 1.1 }, "OTEL_TRACES_SAMPLER_ARG"},
		{"service name", func(c *TracingConfig) { c.ServiceName = " " }, "OTEL_SERVICE_NAME"},
		{"environment", func(c *TracingConfig) { c.DeploymentEnvironment = "" }, "OTEL_DEPLOYMENT_ENVIRONMENT"},
		{"endpoint scheme", func(c *TracingConfig) { c.Endpoint = "http://collector:4317" }, "scheme"},
		{"endpoint no port", func(c *TracingConfig) { c.Endpoint = "collector" }, "host:port"},
		{"endpoint empty", func(c *TracingConfig) { c.Endpoint = "" }, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{"batch timeout", func(c *TracingConfig) { c.BatchTimeout = 0 }, "OTEL_BATCH_TIMEOUT_SECS"},
		{"export timeout", func(c *TracingConfig) { c.ExportTimeout = 0 }, "OTEL_EXPORT_TIMEOUT_SECS"},
		{"shutdown timeout", func(c *TracingConfig) { c.ShutdownTimeout = 0 }, "OTEL_SHUTDOWN_TIMEOUT_SECS"},
		{"queue size", func(c *TracingConfig) { c.MaxQueueSize = 0 }, "OTEL_BATCH_MAX_QUEUE_SIZE"},
		{"batch size", func(c *TracingConfig) { c.MaxExportBatchSize = 0 }, "OTEL_BATCH_MAX_EXPORT_BATCH_SIZE"},
		{"batch larger than queue", func(c *TracingConfig) { c.MaxExportBatchSize = 4096 }, "must not exceed"},
		{"exclude path", func(c *TracingConfig) { c.ExcludePaths = "healthz" }, "must start with '/'"},
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

	// Exporter=none must not require a reachable or well-formed endpoint.
	none := valid()
	none.Exporter = "none"
	none.Endpoint = ""
	if err := none.Validate(); err != nil {
		t.Fatalf("exporter=none rejected an empty endpoint: %v", err)
	}
}

func TestTracingConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := TracingConfig{
		Enabled:      false,
		Exporter:     "nonsense",
		Protocol:     "carrier-pigeon",
		Endpoint:     "not an endpoint",
		Sampler:      "bogus",
		SamplerArg:   99,
		BatchTimeout: -1,
		MaxQueueSize: -1,
		ExcludePaths: "no-slash",
		ServiceName:  "",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled tracing must ignore invalid values, got %v", err)
	}
}

func TestConfigValidatePropagatesTracingError(t *testing.T) {
	cfg := baseValidConfig()
	cfg.Tracing.Enabled = true
	cfg.Tracing.Exporter = "none"
	cfg.Tracing.ServiceName = "api"
	cfg.Tracing.DeploymentEnvironment = "local"
	cfg.Tracing.Sampler = "always_on"
	cfg.Tracing.BatchTimeout = 1
	cfg.Tracing.ExportTimeout = 1
	cfg.Tracing.ShutdownTimeout = 1
	cfg.Tracing.MaxQueueSize = 1
	cfg.Tracing.MaxExportBatchSize = 1
	cfg.Tracing.Protocol = "http"

	if err := cfg.Validate(); err == nil {
		t.Fatal("Config.Validate() must reject an invalid tracing protocol")
	}
}

func TestTracingConfigExcludedPaths(t *testing.T) {
	cfg := TracingConfig{ExcludePaths: " /healthz , /metrics,,/readyz "}
	got := cfg.ExcludedPaths()
	if len(got) != 3 {
		t.Fatalf("ExcludedPaths() = %v, want three entries", got)
	}
}
