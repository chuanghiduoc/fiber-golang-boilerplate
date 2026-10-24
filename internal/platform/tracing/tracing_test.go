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
		Enabled:               true,
		Exporter:              "none",
		Protocol:              "grpc",
		Endpoint:              "localhost:4317",
		Insecure:              true,
		ServiceName:           "test-service",
		ServiceNamespace:      "test-namespace",
		ServiceVersion:        "1.2.3",
		DeploymentEnvironment: "test",
		Sampler:               "parentbased_traceidratio",
		SamplerArg:            1,
		ExcludePaths:          "/healthz,/metrics",
		InstrumentDB:          true,
		BatchTimeout:          1,
		ExportTimeout:         1,
		ShutdownTimeout:       1,
		MaxQueueSize:          16,
		MaxExportBatchSize:    8,
	}
}

func TestNewDisabledIsNoop(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestSamplerValidationAndDecisions(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Sampler = "not-a-sampler"
	_, err := New(context.Background(), cfg)
	require.Error(t, err)

	cfg = testConfig()
	cfg.SamplerArg = 1.5
	_, err = New(context.Background(), cfg)
	require.Error(t, err)

	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	params := sdktrace.SamplingParameters{TraceID: traceID, Name: "test"}

	sampler, name, err := newSampler("parentbased_traceidratio", 0)
	require.NoError(t, err)
	assert.Equal(t, "parentbased_traceidratio(0)", name)
	assert.Equal(t, sdktrace.Drop, sampler.ShouldSample(params).Decision)

	sampler, name, err = newSampler("parentbased_traceidratio", 1)
	require.NoError(t, err)
	assert.Equal(t, "parentbased_traceidratio(1)", name)
	assert.Equal(t, sdktrace.RecordAndSample, sampler.ShouldSample(params).Decision)

	// A sampled parent wins over the root ratio, which is what keeps traces
	// complete when a downstream service samples more conservatively.
	params.ParentContext = trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
	assert.Equal(t, sdktrace.RecordAndSample, sampler.ShouldSample(params).Decision)

	sampler, name, err = newSampler("always_off", 0)
	require.NoError(t, err)
	assert.Equal(t, "always_off", name)
	assert.Equal(t, sdktrace.Drop, sampler.ShouldSample(sdktrace.SamplingParameters{TraceID: traceID}).Decision)

	_, _, err = newSampler("unknown", 0)
	require.Error(t, err)
}

func TestTraceparentRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.Sampler = "always_on"
	provider, err := New(context.Background(), cfg)
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
	t.Parallel()

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
	t.Parallel()

	cfg := testConfig()
	cfg.Exporter = "otlp"
	cfg.Endpoint = "127.0.0.1:1" // nothing listens here
	cfg.Insecure = true

	start := time.Now()
	provider, err := New(context.Background(), cfg)
	require.NoError(t, err, "an unreachable collector must never fail startup")
	assert.Less(t, time.Since(start), 2*time.Second, "provider creation must not dial synchronously")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	// Export failure is allowed here; the point is that shutdown returns and
	// never blocks startup or process liveness.
	_ = provider.Shutdown(shutdownCtx)
	assert.True(t, provider.Enabled())
}

func TestResourceAttributes(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.InstanceID = "instance-1"
	res := newResource(cfg)
	attrs := res.Set()

	for _, key := range []attribute.Key{
		semconv.ServiceNameKey,
		semconv.ServiceNamespaceKey,
		semconv.ServiceVersionKey,
		semconv.ServiceInstanceIDKey,
		semconv.DeploymentEnvironmentNameKey,
	} {
		value, ok := attrs.Value(key)
		require.True(t, ok, "missing resource attribute %s", key)
		assert.NotEmpty(t, value.AsString())
	}

	name, _ := attrs.Value(semconv.ServiceNameKey)
	assert.Equal(t, "test-service", name.AsString())
	env, _ := attrs.Value(semconv.DeploymentEnvironmentNameKey)
	assert.Equal(t, "test", env.AsString())
	instance, _ := attrs.Value(semconv.ServiceInstanceIDKey)
	assert.Equal(t, "instance-1", instance.AsString())

	// Empty instance IDs fall back to the hostname instead of an empty value.
	cfg.InstanceID = ""
	res = newResource(cfg)
	instance, _ = res.Set().Value(semconv.ServiceInstanceIDKey)
	assert.NotEmpty(t, instance.AsString())
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
