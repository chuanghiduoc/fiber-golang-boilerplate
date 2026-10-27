// Package tracing configures the OpenTelemetry SDK used by the API. Tracing is
// disabled by default: the application then uses a no-op provider and never
// opens a network connection. When enabled, spans are batched by the SDK and
// exported over OTLP. An unreachable collector only drops spans; it never
// fails startup or readiness.
//
// The SDK owns its standard environment variables: OTEL_EXPORTER_OTLP_*
// (endpoint URL, protocol, TLS/insecure, headers, timeout), OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_TRACES_SAMPLER/ARG, and OTEL_BSP_*. This
// package only chooses the OTLP transport (http/protobuf by default, gRPC when
// requested) and adds the build version as service.version.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// maxErrorTypeLength bounds the error.type attribute so a pathological type
// name cannot create high-cardinality telemetry.
const maxErrorTypeLength = 64

// Provider owns the tracer provider lifecycle. A nil *Provider is valid and
// behaves like a disabled provider, so callers do not need nil checks.
type Provider struct {
	enabled  bool
	tp       trace.TracerProvider
	sdk      *sdktrace.TracerProvider
	exporter string
	sampler  string
	resource *resource.Resource

	shutdownOnce sync.Once
	shutdownErr  error
}

type options struct {
	serviceVersion string
	exporter       sdktrace.SpanExporter
}

// Option configures New.
type Option func(*options)

// WithServiceVersion adds the build version as resource service.version. It
// overrides a service.version supplied through OTEL_RESOURCE_ATTRIBUTES.
func WithServiceVersion(version string) Option {
	return func(o *options) { o.serviceVersion = version }
}

// withSpanExporter replaces the OTLP exporter with an in-memory one. It exists
// for tests and is not part of the deployment surface.
func withSpanExporter(exporter sdktrace.SpanExporter) Option {
	return func(o *options) { o.exporter = exporter }
}

// New builds a tracer provider from cfg. It validates the application switches
// again because callers may construct a Provider directly (tests, tools)
// without going through config.Load. The OTLP exporter connects lazily, so an
// unreachable collector does not fail here.
func New(ctx context.Context, cfg config.TracingConfig, opts ...Option) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var options options
	for _, opt := range opts {
		opt(&options)
	}

	res, err := newResource(options.serviceVersion)
	if err != nil {
		return nil, err
	}

	if !cfg.Enabled {
		return &Provider{
			tp:       noop.NewTracerProvider(),
			sampler:  "disabled",
			exporter: "none",
			resource: res,
		}, nil
	}

	exporterName, err := tracesExporter()
	if err != nil {
		return nil, err
	}

	providerOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	switch {
	case options.exporter != nil:
		// In-memory exporters must export synchronously so a test can read the
		// recorded spans without a batch delay.
		providerOpts = append(providerOpts, sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(options.exporter)))
	case exporterName == "none":
		// Sampling and propagation still work, but spans are never exported.
		// This is useful for local debugging without a collector.
	case exporterName == "otlp":
		exporter, err := newOTLPExporter(ctx)
		if err != nil {
			return nil, err
		}
		// No batch options: the SDK reads OTEL_BSP_* itself.
		providerOpts = append(providerOpts, sdktrace.WithBatcher(exporter))
	}

	sdkProvider := sdktrace.NewTracerProvider(providerOpts...)
	return &Provider{
		enabled:  true,
		tp:       sdkProvider,
		sdk:      sdkProvider,
		exporter: exporterName,
		sampler:  samplerName(),
		resource: res,
	}, nil
}

// newOTLPExporter selects the OTLP transport from the standard
// OTEL_EXPORTER_OTLP_TRACES_PROTOCOL / OTEL_EXPORTER_OTLP_PROTOCOL variables.
// Endpoint, TLS, headers, compression, and timeouts are left to the exporter's
// own environment handling.
func newOTLPExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	protocol, err := otlpProtocol("TRACES")
	if err != nil {
		return nil, err
	}
	switch protocol {
	case "http/protobuf":
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("tracing: create OTLP/HTTP exporter: %w", err)
		}
		return exporter, nil
	case "grpc":
		exporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("tracing: create OTLP/gRPC exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("tracing: unsupported OTLP protocol %q", protocol)
	}
}

func tracesExporter() (string, error) {
	switch value := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER"))); value {
	case "", "otlp":
		return "otlp", nil
	case "none":
		return "none", nil
	default:
		return "", fmt.Errorf("tracing: OTEL_TRACES_EXPORTER must be otlp or none (got %q)", value)
	}
}

// otlpProtocol resolves the signal-specific protocol before the common one,
// defaulting to http/protobuf as the OTel specification requires.
func otlpProtocol(signal string) (string, error) {
	keys := []string{
		"OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
	}
	for _, key := range keys {
		value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
		if value == "" {
			continue
		}
		switch value {
		case "http/protobuf", "grpc":
			return value, nil
		default:
			return "", fmt.Errorf("tracing: %s must be http/protobuf or grpc (got %q)", key, value)
		}
	}
	return "http/protobuf", nil
}

// samplerName mirrors the SDK's environment sampler for a startup log line.
func samplerName() string {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	if name == "" {
		return "parentbased_always_on"
	}
	if arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); arg != "" {
		return name + "(" + arg + ")"
	}
	return name
}

// Enabled reports whether spans are being generated and exported.
func (p *Provider) Enabled() bool {
	return p != nil && p.enabled
}

// Exporter describes the configured exporter for a startup log line.
func (p *Provider) Exporter() string {
	if p == nil {
		return "none"
	}
	return p.exporter
}

// Sampler describes the active sampler for a startup log line.
func (p *Provider) Sampler() string {
	if p == nil {
		return "disabled"
	}
	return p.sampler
}

// Resource returns the SDK resource reported on every span.
func (p *Provider) Resource() *resource.Resource {
	if p == nil {
		return resource.Empty()
	}
	return p.resource
}

// TracerProvider returns the SDK provider, or a no-op provider when tracing is
// disabled or the receiver is nil.
func (p *Provider) TracerProvider() trace.TracerProvider {
	if p == nil || p.tp == nil {
		return noop.NewTracerProvider()
	}
	return p.tp
}

// Tracer returns a tracer scoped to name.
func (p *Provider) Tracer(name string) trace.Tracer {
	return p.TracerProvider().Tracer(name)
}

// Shutdown flushes pending spans and stops the exporter. It is idempotent: the
// first result is returned on every subsequent call, including concurrent
// calls. A nil or disabled provider is a no-op.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || !p.enabled {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		if p.sdk != nil {
			p.shutdownErr = p.sdk.Shutdown(ctx)
		}
	})
	return p.shutdownErr
}

// newResource merges the SDK default resource (which reads OTEL_SERVICE_NAME
// and OTEL_RESOURCE_ATTRIBUTES) with the build version. The build version wins
// so an image labels its spans correctly even when the environment is stale.
func newResource(version string) (*resource.Resource, error) {
	defaults := resource.Default()
	version = strings.TrimSpace(version)
	if version == "" {
		return defaults, nil
	}
	merged, err := resource.Merge(defaults, resource.NewSchemaless(semconv.ServiceVersion(version)))
	if err != nil {
		return nil, fmt.Errorf("tracing: merge resource: %w", err)
	}
	return merged, nil
}
