package database

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

func newDatabaseTestProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return provider, recorder
}

func TestQueryTracerRecordsSanitizedSpan(t *testing.T) {
	provider, recorder := newDatabaseTestProvider(t)
	tracer := &queryTracer{tp: provider}

	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "request")

	const query = "  -- leading comment\n SELECT * FROM users WHERE email = 'user@example.com' AND token = 'super-secret'"
	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{
		SQL:  query,
		Args: []any{"user@example.com", "super-secret"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	parent.End()

	ended := recorder.Ended()
	require.Len(t, ended, 2) // database span + parent
	span := ended[0]
	require.Equal(t, "db SELECT", span.Name())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	assert.Equal(t, "postgresql", attrString(span.Attributes(), semconv.DBSystemNameKey))
	assert.Equal(t, "SELECT", attrString(span.Attributes(), semconv.DBOperationNameKey))
	assert.Equal(t, codes.Unset, span.Status().Code)

	for _, attr := range span.Attributes() {
		value := attr.Value.String()
		assert.NotContains(t, value, "FROM users")
		assert.NotContains(t, value, "user@example.com")
		assert.NotContains(t, value, "super-secret")
	}
	assert.Empty(t, span.Events())
}

func TestQueryTracerSkipsWithoutRecordingParent(t *testing.T) {
	provider, recorder := newDatabaseTestProvider(t)
	tracer := &queryTracer{tp: provider}

	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	assert.Empty(t, recorder.Ended(), "queries outside a recording span must not create spans")
}

func TestQueryTracerNoRowsIsNotAnError(t *testing.T) {
	provider, recorder := newDatabaseTestProvider(t)
	tracer := &queryTracer{tp: provider}

	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})
	parent.End()

	ended := recorder.Ended()
	require.Len(t, ended, 2)
	span := ended[0]
	assert.Equal(t, codes.Unset, span.Status().Code)
	assert.Empty(t, attrString(span.Attributes(), semconv.ErrorTypeKey))
	assert.Empty(t, attrString(span.Attributes(), semconv.DBResponseStatusCodeKey))
}

func TestQueryTracerRecordsSQLStateWithoutMessage(t *testing.T) {
	provider, recorder := newDatabaseTestProvider(t)
	tracer := &queryTracer{tp: provider}

	parentCtx, parent := provider.Tracer("test").Start(context.Background(), "request")

	ctx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{
		SQL:  "INSERT INTO users (email) VALUES ($1)",
		Args: []any{"user@example.com"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{
		Code:    "23505",
		Message: `duplicate key value violates unique constraint "users_email_key" for user@example.com`,
	}})
	parent.End()

	ended := recorder.Ended()
	require.Len(t, ended, 2)
	span := ended[0]
	assert.Equal(t, "db INSERT", span.Name())
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Empty(t, span.Status().Description)
	assert.Equal(t, "23505", attrString(span.Attributes(), semconv.DBResponseStatusCodeKey))
	assert.Equal(t, "postgres.23505", attrString(span.Attributes(), semconv.ErrorTypeKey))
	for _, attr := range span.Attributes() {
		assert.NotContains(t, attr.Value.String(), "user@example.com")
		assert.NotContains(t, attr.Value.String(), "users_email_key")
	}
	assert.Empty(t, span.Events())
}

func TestSQLOperation(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"select", "SELECT id FROM users", "SELECT"},
		{"lowercase", "  insert into jobs values ($1)", "INSERT"},
		{"line comment", "-- comment\nUPDATE users SET name = $1", "UPDATE"},
		{"block comment", "/* comment */ DELETE FROM files", "DELETE"},
		{"unterminated comment", "/* no end", "UNKNOWN"},
		{"empty", "   ", "UNKNOWN"},
		{"parenthesized", "(SELECT 1)", "UNKNOWN"},
		{"bounded", strings.Repeat("A", 100), strings.Repeat("A", maxOperationLength)},
		{"strips punctuation", "SELECT; DROP TABLE users", "SELECT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sqlOperation(tt.query))
		})
	}
}

func TestSQLStateIsBoundedAndFiltered(t *testing.T) {
	assert.Equal(t, "23505", sqlState("23505"))
	assert.Equal(t, "23505", sqlState(" 23505 "))
	assert.Equal(t, "", sqlState("23505 -- message user@example.com"))
	assert.Equal(t, "", sqlState("not valid"))
	assert.Equal(t, "", sqlState("123456789"))
}

func TestWithTracerProviderOption(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/db?sslmode=disable")
	require.NoError(t, err)
	require.Nil(t, cfg.ConnConfig.Tracer)

	provider, _ := newDatabaseTestProvider(t)
	WithTracerProvider(provider)(cfg)
	require.NotNil(t, cfg.ConnConfig.Tracer)

	// nil and nil config must be inert rather than panic.
	WithTracerProvider(nil)(cfg)
	WithTracerProvider(provider)(nil)

	// An existing tracer is preserved through pgx's multitracer.
	cfg2, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/db?sslmode=disable")
	require.NoError(t, err)
	existing := &stubQueryTracer{}
	cfg2.ConnConfig.Tracer = existing
	WithTracerProvider(provider)(cfg2)
	require.NotNil(t, cfg2.ConnConfig.Tracer)
	assert.NotSame(t, existing, cfg2.ConnConfig.Tracer)
}

type stubQueryTracer struct{}

func (*stubQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*stubQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func attrString(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsString()
		}
	}
	return ""
}
