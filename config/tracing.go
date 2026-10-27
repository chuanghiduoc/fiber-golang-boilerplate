package config

import (
	"fmt"
	"strings"
	"time"
)

// TracingConfig holds the application-specific tracing switches. Everything
// about the SDK pipeline itself (exporter endpoint, protocol, TLS, sampler,
// batching, resource attributes, service name) is read from the standard
// OpenTelemetry environment variables by the SDK, so those settings are not
// duplicated here. See docs/tracing.md.
type TracingConfig struct {
	// Enabled is the app switch; TRACING_ is used so it cannot be confused
	// with an SDK variable.
	Enabled bool `env:"TRACING_ENABLED" envDefault:"false"`
	// ExcludePaths lists exact request paths (for example health and metrics
	// probes) that must not create spans.
	ExcludePaths []string `env:"TRACING_EXCLUDE_PATHS" envSeparator:"," envDefault:"/healthz,/readyz,/health,/metrics"`
	// InstrumentDB adds sanitized client spans for pgx queries that run under
	// a recording parent span.
	InstrumentDB bool `env:"TRACING_INSTRUMENT_DB" envDefault:"true"`
	// TrustIncoming honors an incoming W3C traceparent. It is off by default
	// because a client-supplied sampled flag would force sampling under the
	// parent-based sampler. With APP_TRUST_PROXY=true it is honored only from
	// peers in APP_TRUSTED_PROXIES.
	TrustIncoming bool `env:"TRACING_TRUST_INCOMING_TRACE" envDefault:"false"`
	// ShutdownTimeout bounds the final span flush.
	ShutdownTimeout time.Duration `env:"TRACING_SHUTDOWN_TIMEOUT" envDefault:"10s"`
}

// ExcludedPaths returns the parsed list of paths that are not traced.
func (t TracingConfig) ExcludedPaths() []string {
	return trimmedList(t.ExcludePaths)
}

// Validate checks tracing settings only when tracing is enabled. A disabled
// configuration is inert, so stale or partial values cannot break a
// deployment that does not use tracing.
func (t TracingConfig) Validate() error {
	if !t.Enabled {
		return nil
	}
	if t.ShutdownTimeout < time.Second {
		return fmt.Errorf("TRACING_SHUTDOWN_TIMEOUT must be at least 1s")
	}
	return t.validateExcludedPaths()
}

// validateExcludedPaths requires absolute request paths.
func (t TracingConfig) validateExcludedPaths() error {
	for _, path := range t.ExcludedPaths() {
		if !strings.HasPrefix(path, "/") {
			return fmt.Errorf("TRACING_EXCLUDE_PATHS entries must start with '/' (got %q)", path)
		}
	}
	return nil
}
