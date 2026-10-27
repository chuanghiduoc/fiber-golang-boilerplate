package config

// MetricsConfig holds the metric-pipeline switches. Prometheus rendering on
// /metrics is always on; OTLP is additive and reads the standard
// OTEL_EXPORTER_OTLP_* and OTEL_METRIC_EXPORT_* variables itself.
type MetricsConfig struct {
	// OTLPEnabled adds an OTLP metric reader. OTEL_METRICS_EXPORTER=otlp also
	// enables it, and OTEL_METRICS_EXPORTER=none forces it off.
	OTLPEnabled bool `env:"METRICS_OTLP_ENABLED" envDefault:"false"`
}

// Validate checks the metric switches. The OTLP transport protocol is
// validated when the exporter is constructed, so a bad protocol fails startup
// instead of exporting nowhere.
func (m MetricsConfig) Validate() error {
	return nil
}
