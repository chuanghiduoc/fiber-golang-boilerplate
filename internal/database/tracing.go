package database

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	maxOperationLength  = 32
)

// querySpanKey carries the active span from TraceQueryStart to TraceQueryEnd.
// A private key prevents collisions with application context values.
type querySpanKey struct{}

// queryTracer implements pgx.QueryTracer. It intentionally exposes only the
// statement class and SQLSTATE: queries, bind parameters, and driver error
// messages routinely contain PII, tokens, or uploaded file metadata.
type queryTracer struct {
	tp trace.TracerProvider
}

var _ pgx.QueryTracer = (*queryTracer)(nil)

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if t == nil || t.tp == nil || ctx == nil {
		return ctx
	}
	// Only contribute spans when an instrumented operation is already
	// recording. Background work without a root span must not create orphan
	// database traces or pay the sampling cost.
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}

	operation := sqlOperation(data.SQL)
	ctx, span := t.tp.Tracer(instrumentationName).Start(ctx, "db "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationNameKey.String(operation),
		),
	)
	return context.WithValue(ctx, querySpanKey{}, span)
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx == nil {
		return
	}
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok || span == nil {
		return
	}
	defer span.End()

	err := data.Err
	if err == nil || errors.Is(err, pgx.ErrNoRows) {
		// A miss is a normal application outcome, not an infrastructure error.
		return
	}

	attrs := []attribute.KeyValue{semconv.ErrorTypeKey.String("database_error")}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if code := sqlState(pgErr.Code); code != "" {
			attrs = append(attrs,
				semconv.DBResponseStatusCode(code),
				semconv.ErrorTypeKey.String("postgres."+code),
			)
		}
	}
	span.SetAttributes(attrs...)
	span.SetStatus(codes.Error, "")
}

// sqlOperation extracts the leading SQL keyword without retaining statement
// text. Whitespace and comments are skipped and the result is restricted to a
// short alphanumeric token.
func sqlOperation(sql string) string {
	rest := strings.TrimSpace(sql)
	for {
		switch {
		case strings.HasPrefix(rest, "--"):
			if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
				rest = strings.TrimSpace(rest[newline+1:])
				continue
			}
			return "UNKNOWN"
		case strings.HasPrefix(rest, "/*"):
			if end := strings.Index(rest, "*/"); end >= 0 {
				rest = strings.TrimSpace(rest[end+2:])
				continue
			}
			return "UNKNOWN"
		}
		break
	}

	end := 0
	for end < len(rest) && end < maxOperationLength {
		c := rest[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return "UNKNOWN"
	}
	return strings.ToUpper(rest[:end])
}

// sqlState bounds and filters a PostgreSQL SQLSTATE value before it becomes an
// attribute. SQLSTATEs are five alphanumeric characters, so this also strips
// accidental message text from a malformed error.
// sqlState validates a PostgreSQL SQLSTATE before it becomes an attribute.
// SQLSTATEs are exactly five alphanumeric characters, so anything else is
// dropped rather than allowing message text into telemetry.
func sqlState(code string) string {
	code = strings.TrimSpace(code)
	if len(code) != 5 {
		return ""
	}
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return ""
	}
	return code
}

func combineQueryTracers(existing, added pgx.QueryTracer) pgx.QueryTracer {
	return multitracer.New(existing, added)
}
