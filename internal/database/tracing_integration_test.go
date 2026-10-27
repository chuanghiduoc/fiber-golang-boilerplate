package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"go.opentelemetry.io/otel/codes"
)

// TestQueryTracerIntegration verifies the pgx hook against a real server. It
// skips without TEST_DATABASE_URL, so the default unit run stays Docker-free.
func TestQueryTracerIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tracing integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 2
	WithTracerProvider(provider)(cfg)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	parentCtx, parent := provider.Tracer("test").Start(ctx, "request")
	_, err = pool.Exec(parentCtx, "SELECT 1")
	require.NoError(t, err)
	parent.End()

	ended := recorder.Ended()
	require.Len(t, ended, 2)
	span := ended[0]
	assert.Equal(t, "db SELECT", span.Name())
	assert.Equal(t, "SELECT", attrString(span.Attributes(), semconv.DBOperationNameKey))
	assert.Equal(t, codes.Unset, span.Status().Code)
}
