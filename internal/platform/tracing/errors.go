package tracing

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// MarkError marks span as failed using a bounded error class only. It never
// records err.Error(), events, or exception attributes: error messages can
// contain emails, tokens, SQL, or request payloads. Callers that need detail
// should log it separately with the same sanitization rules as request logs.
func MarkError(span trace.Span, err error) {
	if span == nil || !span.IsRecording() || isNil(err) {
		return
	}
	span.SetStatus(codes.Error, "")
	span.SetAttributes(semconv.ErrorTypeKey.String(ErrorType(err)))
}

// ErrorType classifies err into a bounded, low-cardinality value suitable for
// the error.type attribute. It prefers well-known control-flow errors and
// otherwise uses the Go type name; the error text is never inspected.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context.canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.deadline_exceeded"
	}

	// Classify the underlying cause rather than a formatting wrapper such as
	// fmt.wrapError, whose name carries no diagnostic value.
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}

	typ := reflect.TypeOf(err)
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	name := "error"
	if typ != nil && typ.Name() != "" {
		name = typ.Name()
	}
	switch name {
	case "errorString", "wrapError", "joinError", "fundamental":
		name = "application_error"
	}
	return boundedErrorType(name)
}

func boundedErrorType(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == ':':
			b.WriteRune(r)
		default:
			// Drop whitespace, quotes, newlines, and other characters that
			// could smuggle unstructured error text into telemetry.
		}
		if b.Len() >= maxErrorTypeLength {
			break
		}
	}
	out := b.String()
	if out == "" {
		return "error"
	}
	if len(out) > maxErrorTypeLength {
		out = out[:maxErrorTypeLength]
	}
	return out
}

func isNil(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
