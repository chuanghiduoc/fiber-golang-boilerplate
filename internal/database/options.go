package database

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"
)

// PoolOption customizes the pgx pool built by NewPool.
type PoolOption func(*pgxpool.Config)

// WithTracerProvider instruments queries with a sanitized OpenTelemetry client
// span. Only SQL type information (for example SELECT) and PostgreSQL SQLSTATE
// values are recorded; SQL text, arguments, and error messages are never
// attached to spans.
func WithTracerProvider(tp trace.TracerProvider) PoolOption {
	return func(cfg *pgxpool.Config) {
		if cfg == nil || tp == nil {
			return
		}
		tracer := &queryTracer{tp: tp}
		if existing := cfg.ConnConfig.Tracer; existing != nil {
			cfg.ConnConfig.Tracer = combineQueryTracers(existing, tracer)
			return
		}
		cfg.ConnConfig.Tracer = tracer
	}
}
