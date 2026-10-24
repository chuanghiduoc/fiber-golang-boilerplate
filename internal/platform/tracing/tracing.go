// Package tracing configures the OpenTelemetry SDK used by the API. Tracing is
// disabled by default: the application then uses a no-op provider and never
// opens a network connection. When enabled, spans are batched in memory and
// exported over OTLP/gRPC. An unreachable collector only drops spans; it never
// fails startup or readiness.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
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
	enabled bool
	tp      trace.TracerProvider
	sdk     *sdktrace.TracerProvider
	sampler string

	shutdownOnce sync.Once
	shutdownErr  error
}

// New builds a tracer provider from cfg. It validates the configuration again
// because callers may construct a Provider directly (tests, tools) without
// going through config.Load. The OTLP exporter connects lazily, so an
// unreachable collector does not fail here.
func New(ctx context.Context, cfg config.TracingConfig) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return &Provider{tp: noop.NewTracerProvider(), sampler: "disabled"}, nil
	}

	sampler, samplerName, err := newSampler(cfg.Sampler, cfg.SamplerArg)
	if err != nil {
		return nil, err
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sampler),
		sdktrace.WithResource(newResource(cfg)),
	}

	switch strings.ToLower(strings.TrimSpace(cfg.Exporter)) {
	case "otlp":
		exporter, err := newOTLPExporter(ctx, cfg)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(time.Duration(cfg.BatchTimeout)*time.Second),
			sdktrace.WithExportTimeout(time.Duration(cfg.ExportTimeout)*time.Second),
			sdktrace.WithMaxQueueSize(cfg.MaxQueueSize),
			sdktrace.WithMaxExportBatchSize(cfg.MaxExportBatchSize),
		))
	case "none":
		// Sampling and propagation still work, but spans are never exported.
		// This is useful for local debugging without a collector.
	default:
		return nil, fmt.Errorf("tracing: unsupported exporter %q", cfg.Exporter)
	}

	sdkProvider := sdktrace.NewTracerProvider(opts...)
	return &Provider{
		enabled: true,
		tp:      sdkProvider,
		sdk:     sdkProvider,
		sampler: samplerName,
	}, nil
}

func newOTLPExporter(ctx context.Context, cfg config.TracingConfig) (*otlptrace.Exporter, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(strings.TrimSpace(cfg.Endpoint)),
		otlptracegrpc.WithTimeout(time.Duration(cfg.ExportTimeout) * time.Second),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP/gRPC exporter: %w", err)
	}
	return exporter, nil
}

// Enabled reports whether spans are being generated and exported.
func (p *Provider) Enabled() bool {
	return p != nil && p.enabled
}

// Sampler describes the active sampler for a startup log line.
func (p *Provider) Sampler() string {
	if p == nil {
		return "disabled"
	}
	return p.sampler
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

func newResource(cfg config.TracingConfig) *resource.Resource {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(strings.TrimSpace(cfg.ServiceName)),
		semconv.DeploymentEnvironmentName(strings.TrimSpace(cfg.DeploymentEnvironment)),
	}
	if version := strings.TrimSpace(cfg.ServiceVersion); version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	if namespace := strings.TrimSpace(cfg.ServiceNamespace); namespace != "" {
		attrs = append(attrs, semconv.ServiceNamespace(namespace))
	}

	instance := strings.TrimSpace(cfg.InstanceID)
	if instance == "" {
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			instance = "unknown"
		} else {
			instance = hostname
		}
	}
	attrs = append(attrs, semconv.ServiceInstanceID(instance))

	return resource.NewWithAttributes(semconv.SchemaURL, attrs...)
}

func newSampler(name string, arg float64) (sdktrace.Sampler, string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(arg)),
			fmt.Sprintf("parentbased_traceidratio(%g)", arg), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), "parentbased_always_on", nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), "parentbased_always_off", nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(arg), fmt.Sprintf("traceidratio(%g)", arg), nil
	case "always_on":
		return sdktrace.AlwaysSample(), "always_on", nil
	case "always_off":
		return sdktrace.NeverSample(), "always_off", nil
	default:
		return nil, "", fmt.Errorf("tracing: unsupported sampler %q", name)
	}
}
