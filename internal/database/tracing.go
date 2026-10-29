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
// batchSpanKey carries the per-query spans for a SendBatch call until their
// results are consumed. Private key types prevent collisions with application
// context values.
type querySpanKey struct{}
type batchSpanKey struct{}

type batchSpanState struct {
	parent  trace.Span
	queries []trace.Span
	next    int
	failed  bool
}

// queryTracer implements pgx.QueryTracer and pgx.BatchTracer. It intentionally
// exposes only the statement class and SQLSTATE: queries, bind parameters, and
// driver error messages routinely contain PII, tokens, or uploaded metadata.
type queryTracer struct {
	tp trace.TracerProvider
}

var (
	_ pgx.QueryTracer = (*queryTracer)(nil)
	_ pgx.BatchTracer = (*queryTracer)(nil)
)

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !t.recording(ctx) {
		return ctx
	}
	ctx, span := t.startSpan(ctx, sqlOperation(data.SQL))
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
	finishSpan(span, data.Err)
}

// TraceBatchStart creates a parent batch span and one child per queued query.
// The child spans end as pgx reports each result. Ending the remaining spans in
// TraceBatchEnd prevents leaks when a caller closes the batch early.
func (t *queryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if !t.recording(ctx) || data.Batch == nil {
		return ctx
	}
	parentCtx, parent := t.startSpan(ctx, "BATCH")
	state := &batchSpanState{
		parent:  parent,
		queries: make([]trace.Span, len(data.Batch.QueuedQueries)),
	}
	for i, query := range data.Batch.QueuedQueries {
		if query == nil {
			continue
		}
		_, span := t.startSpan(parentCtx, sqlOperation(query.SQL))
		state.queries[i] = span
	}
	return context.WithValue(ctx, batchSpanKey{}, state)
}

func (t *queryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	state := batchStateFromContext(ctx)
	if state == nil {
		return
	}
	span := state.take()
	if span == nil {
		return
	}
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		state.failed = true
	}
	finishSpan(span, data.Err)
}

func (t *queryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	state := batchStateFromContext(ctx)
	if state == nil {
		return
	}
	for state.next < len(state.queries) {
		finishSpan(state.take(), nil)
	}
	if data.Err != nil {
		state.failed = true
	}
	if state.failed {
		state.parent.SetStatus(codes.Error, "")
	}
	state.parent.End()
}

func (s *batchSpanState) take() trace.Span {
	if s == nil || s.next >= len(s.queries) {
		return nil
	}
	span := s.queries[s.next]
	s.next++
	return span
}

func batchStateFromContext(ctx context.Context) *batchSpanState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(batchSpanKey{}).(*batchSpanState)
	return state
}

func (t *queryTracer) recording(ctx context.Context) bool {
	if t == nil || t.tp == nil || ctx == nil {
		return false
	}
	// Only contribute spans when an instrumented operation is already
	// recording. Background work without a root span must not create orphan
	// database traces or pay the sampling cost.
	return trace.SpanFromContext(ctx).IsRecording()
}

func (t *queryTracer) startSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return t.tp.Tracer(instrumentationName).Start(ctx, "db "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			semconv.DBSystemNamePostgreSQL,
			semconv.DBOperationNameKey.String(operation),
		),
	)
}

func finishSpan(span trace.Span, err error) {
	if span == nil {
		return
	}
	defer span.End()
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
