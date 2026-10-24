package config

import (
	"fmt"
	"net"
	"strings"
)

// TracingConfig controls OpenTelemetry trace generation and export. Tracing is
// disabled by default; when disabled, the application uses a no-op provider
// and never opens a collector connection.
type TracingConfig struct {
	Enabled          bool   `env:"OTEL_ENABLED" envDefault:"false"`
	Exporter         string `env:"OTEL_TRACES_EXPORTER" envDefault:"otlp"` // otlp|none
	Protocol         string `env:"OTEL_EXPORTER_OTLP_PROTOCOL" envDefault:"grpc"`
	Endpoint         string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	Insecure         bool   `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"true"`
	ServiceName      string `env:"OTEL_SERVICE_NAME" envDefault:"fiber-golang-boilerplate"`
	ServiceNamespace string `env:"OTEL_SERVICE_NAMESPACE"`
	ServiceVersion   string `env:"OTEL_SERVICE_VERSION" envDefault:"dev"`
	// DeploymentEnvironment is the logical environment (local, staging,
	// production) reported on every span resource.
	DeploymentEnvironment string `env:"OTEL_DEPLOYMENT_ENVIRONMENT" envDefault:"local"`
	// InstanceID identifies one process. Empty means the hostname is used.
	InstanceID string `env:"OTEL_SERVICE_INSTANCE_ID"`
	// Sampler accepts the standard OTel names. The default samples every
	// trace at the root, but always follows a parent's sampling decision.
	Sampler    string  `env:"OTEL_TRACES_SAMPLER" envDefault:"parentbased_traceidratio"`
	SamplerArg float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"1.0"`
	// ExcludePaths is a comma-separated list of exact request paths (for
	// example health and metrics probes) that must not create spans.
	ExcludePaths string `env:"OTEL_TRACING_EXCLUDE_PATHS" envDefault:"/healthz,/readyz,/health,/metrics"`
	// InstrumentDB adds sanitized client spans for pgx queries that run under
	// a recording parent span.
	InstrumentDB bool `env:"OTEL_INSTRUMENT_DB" envDefault:"true"`
	// TrustIncoming honors an incoming W3C traceparent. It is off by default
	// because a client-supplied sampled flag would force sampling under the
	// parent-based sampler. With APP_TRUST_PROXY=true it is honored only from
	// peers in APP_TRUSTED_PROXIES.
	TrustIncoming bool `env:"OTEL_TRUST_INCOMING_TRACE" envDefault:"false"`

	BatchTimeout       int `env:"OTEL_BATCH_TIMEOUT_SECS" envDefault:"5"`
	ExportTimeout      int `env:"OTEL_EXPORT_TIMEOUT_SECS" envDefault:"30"`
	ShutdownTimeout    int `env:"OTEL_SHUTDOWN_TIMEOUT_SECS" envDefault:"10"`
	MaxQueueSize       int `env:"OTEL_BATCH_MAX_QUEUE_SIZE" envDefault:"2048"`
	MaxExportBatchSize int `env:"OTEL_BATCH_MAX_EXPORT_BATCH_SIZE" envDefault:"512"`
}

// ExcludedPaths returns the parsed list of paths that are not traced.
func (t TracingConfig) ExcludedPaths() []string {
	return splitCSV(t.ExcludePaths)
}

// Validate checks tracing settings only when tracing is enabled. A disabled
// configuration is inert, so stale or partial OTEL_* values cannot break a
// deployment that does not use tracing.
func (t TracingConfig) Validate() error {
	if !t.Enabled {
		return nil
	}
	return firstError([]func() error{
		t.validateExporter,
		t.validateResource,
		t.validateSampler,
		t.validateEndpoint,
		t.validateBatching,
		t.validateExcludedPaths,
	})
}

// validateExporter checks the exporter kind and protocol.
func (t TracingConfig) validateExporter() error {
	switch strings.ToLower(strings.TrimSpace(t.Exporter)) {
	case "otlp", "none":
	default:
		return fmt.Errorf("OTEL_TRACES_EXPORTER must be one of: otlp, none (got %q)", t.Exporter)
	}
	if proto := strings.ToLower(strings.TrimSpace(t.Protocol)); proto != "grpc" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL must be grpc (got %q)", t.Protocol)
	}
	return nil
}

// validateResource checks the attributes reported on every span resource.
func (t TracingConfig) validateResource() error {
	if strings.TrimSpace(t.ServiceName) == "" {
		return fmt.Errorf("OTEL_SERVICE_NAME must not be empty")
	}
	if strings.TrimSpace(t.DeploymentEnvironment) == "" {
		return fmt.Errorf("OTEL_DEPLOYMENT_ENVIRONMENT must not be empty")
	}
	return nil
}

// validateSampler checks the sampler name and ratio.
func (t TracingConfig) validateSampler() error {
	if strings.TrimSpace(t.Sampler) == "" {
		return fmt.Errorf("OTEL_TRACES_SAMPLER must not be empty")
	}
	switch strings.ToLower(strings.TrimSpace(t.Sampler)) {
	case "parentbased_traceidratio", "parentbased_always_on", "parentbased_always_off",
		"traceidratio", "always_on", "always_off":
	default:
		return fmt.Errorf("OTEL_TRACES_SAMPLER %q is not supported", t.Sampler)
	}
	if t.SamplerArg < 0 || t.SamplerArg > 1 {
		return fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be between 0 and 1 (got %v)", t.SamplerArg)
	}
	return nil
}

// validateEndpoint checks the collector address when exporting over OTLP.
func (t TracingConfig) validateEndpoint() error {
	if !strings.EqualFold(strings.TrimSpace(t.Exporter), "otlp") {
		return nil
	}
	return validateOTLPEndpoint(t.Endpoint)
}

// validateBatching checks the batch span processor bounds.
func (t TracingConfig) validateBatching() error {
	if t.BatchTimeout < 1 {
		return fmt.Errorf("OTEL_BATCH_TIMEOUT_SECS must be at least 1 second")
	}
	if t.ExportTimeout < 1 {
		return fmt.Errorf("OTEL_EXPORT_TIMEOUT_SECS must be at least 1 second")
	}
	if t.ShutdownTimeout < 1 {
		return fmt.Errorf("OTEL_SHUTDOWN_TIMEOUT_SECS must be at least 1 second")
	}
	if t.MaxQueueSize < 1 {
		return fmt.Errorf("OTEL_BATCH_MAX_QUEUE_SIZE must be at least 1")
	}
	if t.MaxExportBatchSize < 1 {
		return fmt.Errorf("OTEL_BATCH_MAX_EXPORT_BATCH_SIZE must be at least 1")
	}
	if t.MaxExportBatchSize > t.MaxQueueSize {
		return fmt.Errorf("OTEL_BATCH_MAX_EXPORT_BATCH_SIZE (%d) must not exceed OTEL_BATCH_MAX_QUEUE_SIZE (%d)",
			t.MaxExportBatchSize, t.MaxQueueSize)
	}
	return nil
}

// validateExcludedPaths requires absolute request paths.
func (t TracingConfig) validateExcludedPaths() error {
	for _, path := range t.ExcludedPaths() {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("OTEL_TRACING_EXCLUDE_PATHS entries must start with '/' (got %q)", path)
		}
	}
	return nil
}

// validateOTLPEndpoint rejects URLs and empty values so a typo fails at
// startup instead of silently exporting nowhere.
func validateOTLPEndpoint(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must not be empty when OTEL_TRACES_EXPORTER=otlp")
	}
	if strings.Contains(endpoint, "://") {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be host:port without a scheme (got %q)", endpoint)
	}
	if _, _, err := net.SplitHostPort(endpoint); err != nil {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be host:port (got %q)", endpoint)
	}
	return nil
}
