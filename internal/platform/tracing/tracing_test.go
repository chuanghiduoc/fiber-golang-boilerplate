package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

func testConfig() config.TracingConfig {
	return config.TracingConfig{
		Enabled:         true,
		ExcludePaths:    []string{"/healthz", "/metrics"},
		InstrumentDB:    true,
		ShutdownTimeout: time.Second,
	}
}

// clearOTELEnv removes the standard SDK variables a developer shell may export
// so each test exercises what it sets explicitly.
func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OTEL_TRACES_EXPORTER",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
		"OTEL_METRICS_EXPORTER",
	} {
		t.Setenv(key, "")
	}
}

func TestNewDisabledIsNoop(t *testing.T) {
	clearOTELEnv(t)

	provider, err := New(context.Background(), config.TracingConfig{})
	require.NoError(t, err)
	require.NotNil(t, provider)
	assert.False(t, provider.Enabled())
	assert.Equal(t, "disabled", provider.Sampler())
	require.NotNil(t, provider.TracerProvider())
	assert.NoError(t, provider.Shutdown(context.Background()))

	_, span := provider.Tracer("test").Start(context.Background(), "disabled")
	assert.False(t, span.IsRecording(), "a disabled provider must not record spans")
	span.End()
	assert.Empty(t, CaptureRemoteContext(context.Background()))
}

func TestNilProviderIsSafe(t *testing.T) {
	var provider *Provider
	assert.False(t, provider.Enabled())
	assert.Equal(t, "disabled", provider.Sampler())
	assert.NotNil(t, provider.TracerProvider())
	assert.NoError(t, provider.Shutdown(context.Background()))

	ctx, span := provider.Tracer("test").Start(context.Background(), "noop")
	assert.False(t, span.IsRecording())
	span.End()
	assert.Empty(t, CaptureRemoteContext(ctx))
}

// TestNewReadsSDKSamplerEnv proves the SDK, not a copied config field, reads
// OTEL_TRACES_SAMPLER/OTEL_TRACES_SAMPLER_ARG.
func TestNewReadsSDKSamplerEnv(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")

	provider, err := New(context.Background(), testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	assert.Equal(t, "always_off", provider.Sampler())

	_, span := provider.Tracer("test").Start(context.Background(), "never")
	assert.False(t, span.IsRecording())

	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	provider, err = New(context.Background(), testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	_, span = provider.Tracer("test").Start(context.Background(), "always")
	assert.True(t, span.IsRecording())
}

func TestExportSelection(t *testing.T) {
	clearOTELEnv(t)

	t.Run("none samples without exporting", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "none")
		provider, err := New(context.Background(), testConfig())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
		assert.Equal(t, "none", provider.Exporter())
		_, span := provider.Tracer("test").Start(context.Background(), "sampled")
		assert.True(t, span.IsRecording())
		span.End()
	})

	t.Run("unsupported exporter fails startup", func(t *testing.T) {
		t.Setenv("OTEL_TRACES_EXPORTER", "carrier-pigeon")
		_, err := New(context.Background(), testConfig())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OTEL_TRACES_EXPORTER")
	})

	t.Run("unsupported protocol fails startup", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "http/json")
		_, err := New(context.Background(), testConfig())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "OTEL_EXPORTER_OTLP")
	})

	t.Run("protocol defaults to http/protobuf", func(t *testing.T) {
		clearOTELEnv(t)
		protocol, err := otlpProtocol("TRACES")
		require.NoError(t, err)
		assert.Equal(t, "http/protobuf", protocol)
	})

	t.Run("grpc protocol is honored", func(t *testing.T) {
		clearOTELEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
		protocol, err := otlpProtocol("TRACES")
		require.NoError(t, err)
		assert.Equal(t, "grpc", protocol)
	})
}

func TestTraceparentRoundTrip(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	provider, err := New(context.Background(), testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	ctx, span := provider.Tracer("test").Start(context.Background(), "roundtrip")
	defer span.End()

	header := CaptureRemoteContext(ctx)
	require.NotEmpty(t, header)
	assert.True(t, strings.HasPrefix(header, "00-"), "expected W3C traceparent, got %q", header)

	extracted := ExtractRemoteContext(context.Background(), header)
	remote := trace.SpanContextFromContext(extracted)
	require.True(t, remote.IsValid())
	assert.True(t, remote.IsRemote())
	assert.Equal(t, span.SpanContext().TraceID(), remote.TraceID())
	assert.Equal(t, span.SpanContext().SpanID(), remote.SpanID())
	assert.Equal(t, span.SpanContext().TraceFlags(), remote.TraceFlags())

	malformed := ExtractRemoteContext(context.Background(), "not-a-traceparent")
	assert.False(t, trace.SpanContextFromContext(malformed).IsValid())

	empty := ExtractRemoteContext(context.Background(), "")
	assert.False(t, trace.SpanContextFromContext(empty).IsValid())
}

func TestShutdownIsIdempotent(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")

	provider, err := New(context.Background(), testConfig())
	require.NoError(t, err)
	require.True(t, provider.Enabled())

	const calls = 8
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- provider.Shutdown(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	assert.NoError(t, provider.Shutdown(context.Background()), "shutdown must stay idempotent")
}

func TestNewWithUnreachableCollectorDoesNotFail(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	start := time.Now()
	provider, err := New(context.Background(), testConfig())
	require.NoError(t, err, "an unreachable collector must never fail startup")
	assert.Less(t, time.Since(start), 2*time.Second, "provider creation must not dial synchronously")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	// Export failure is allowed here; the point is that shutdown returns and
	// never blocks startup or process liveness.
	_ = provider.Shutdown(shutdownCtx)
	assert.True(t, provider.Enabled())
}

func TestResourceUsesBuildVersion(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	provider, err := New(context.Background(), testConfig(), WithServiceVersion("1.2.3"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	attrs := provider.Resource().Set()
	version, ok := attrs.Value(semconv.ServiceVersionKey)
	require.True(t, ok, "service.version must be present")
	assert.Equal(t, "1.2.3", version.AsString())

	name, ok := attrs.Value(semconv.ServiceNameKey)
	require.True(t, ok, "service.name must come from the SDK default resource")
	assert.NotEmpty(t, name.AsString())
}

// TestInMemoryExporterEndToEnd is the in-memory exporter check: the provider
// built from configuration records and exports to a tracetest recorder.
func TestInMemoryExporterEndToEnd(t *testing.T) {
	clearOTELEnv(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

	exporter := tracetest.NewInMemoryExporter()
	provider, err := New(context.Background(), testConfig(),
		WithServiceVersion("1.2.3"), withSpanExporter(exporter))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	_, span := provider.Tracer("test").Start(context.Background(), "in-memory")
	span.SetAttributes(semconv.HTTPRequestMethodKey.String("GET"))
	span.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, "in-memory", spans[0].Name)
	assert.Equal(t, "1.2.3", resourceAttribute(spans[0].Resource.Set(), semconv.ServiceVersionKey))
}

func TestMarkErrorIsBoundedAndPIIFree(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	_, span := provider.Tracer("test").Start(context.Background(), "failed")
	secret := errors.New("smtp rejected user@example.com with token=super-secret")
	MarkError(span, secret)
	span.End()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, codes.Error, ended[0].Status().Code)
	assert.Empty(t, ended[0].Status().Description, "status description must not contain the error message")
	assert.Empty(t, ended[0].Events(), "raw errors must not be recorded as events")
	assert.Equal(t, "application_error", attributeValue(ended[0].Attributes(), semconv.ErrorTypeKey))

	for _, attr := range ended[0].Attributes() {
		assert.NotContains(t, attr.Value.AsString(), "user@example.com")
		assert.NotContains(t, attr.Value.AsString(), "super-secret")
	}
}

func TestMarkErrorClassifiesKnownErrors(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "context.deadline_exceeded", ErrorType(context.DeadlineExceeded))
	assert.Equal(t, "context.canceled", ErrorType(context.Canceled))
	assert.Equal(t, "testError", ErrorType(testError{}))
	assert.Equal(t, "testError", ErrorType(fmt.Errorf("wrapped: %w", testError{})))
	assert.Equal(t, "", ErrorType(nil))

	assert.LessOrEqual(t, len(boundedErrorType(strings.Repeat("A", 200))), maxErrorTypeLength)
	assert.Equal(t, "a.b_c-d:e", boundedErrorType("a.b_c-d:e"))
	assert.Equal(t, "error", boundedErrorType(" \n\t "))
}

func TestMarkErrorNilAndNoopSpansAreSafe(t *testing.T) {
	t.Parallel()

	MarkError(nil, errors.New("ignored"))
	MarkError(trace.SpanFromContext(context.Background()), errors.New("ignored"))
}

type testError struct{}

func (testError) Error() string { return "test error" }

func attributeValue(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

func resourceAttribute(attrs *attribute.Set, key attribute.Key) string {
	value, ok := attrs.Value(key)
	if !ok {
		return ""
	}
	return value.AsString()
}
