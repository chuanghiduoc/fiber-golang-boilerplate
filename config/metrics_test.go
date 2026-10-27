package config

import (
	"testing"

	"github.com/caarlos0/env/v11"
)

func TestMetricsConfigDefaults(t *testing.T) {
	t.Setenv("METRICS_OTLP_ENABLED", "")

	var cfg MetricsConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse metrics defaults: %v", err)
	}
	if cfg.OTLPEnabled {
		t.Error("OTLP metrics export must be disabled by default")
	}
}

func TestMetricsConfigOTLP(t *testing.T) {
	t.Setenv("METRICS_OTLP_ENABLED", "true")

	var cfg MetricsConfig
	if err := env.Parse(&cfg); err != nil {
		t.Fatalf("parse metrics: %v", err)
	}
	if !cfg.OTLPEnabled {
		t.Error("METRICS_OTLP_ENABLED=true must enable OTLP metrics export")
	}
}
