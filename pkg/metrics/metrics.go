// Package metrics owns the process metric instruments. Business code records
// through the package-level instruments; the OpenTelemetry SDK renders them
// for the Prometheus /metrics endpoint and, when enabled, exports the same
// measurements over OTLP.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// instrumentationScope identifies this application's instruments in the OTLP
// resource/scope model.
const instrumentationScope = "github.com/chuanghiduoc/fiber-golang-boilerplate"

// httpServerDurationBuckets are the buckets the OTel HTTP semantic conventions
// recommend for http.server.request.duration (seconds).
var httpServerDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

// Options configures a metric provider.
type Options struct {
	// ServiceVersion becomes the resource service.version attribute. The build
	// sets it; an empty value leaves the SDK default resource untouched.
	ServiceVersion string
	// OTLPEnabled adds an OTLP metric reader in addition to the always-on
	// Prometheus reader. The standard OTEL_EXPORTER_OTLP_* environment
	// variables configure its endpoint, TLS, headers, and protocol.
	OTLPEnabled bool
}

// Provider owns the meter provider and its readers.
type Provider struct {
	meterProvider *sdkmetric.MeterProvider
	registry      *prometheus.Registry

	shutdownOnce sync.Once
	shutdownErr  error
}

type meterState struct {
	gen      uint64
	meter    otelmetric.Meter
	provider *Provider
}

var (
	current     atomic.Pointer[meterState]
	generation  atomic.Uint64
	setupMu     sync.Mutex
	instrumentM sync.Mutex
)

// init installs the default Prometheus-only pipeline so instruments are live
// even in tools and tests that never load the application config. cmd/api
// calls Setup again to add the OTLP reader; instruments rebind to the new
// provider generation on their next use.
func init() {
	if _, err := Setup(Options{}); err != nil {
		slog.Error("metrics: initialize default pipeline", slog.Any("error", err))
	}
}

// Setup builds the OpenTelemetry metric pipeline. It is called once at process
// startup; a second call replaces the provider, which tests use to get a fresh
// registry. The Prometheus reader is always present so /metrics never
// disappears; OTLP is additive.
func Setup(opts Options) (*Provider, error) {
	setupMu.Lock()
	defer setupMu.Unlock()

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	promExporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
		otelprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create Prometheus exporter: %w", err)
	}

	res, err := buildResource(opts.ServiceVersion)
	if err != nil {
		return nil, err
	}

	readerOpts := []sdkmetric.Option{
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	}
	if otlpRequested(opts.OTLPEnabled) {
		exporter, err := newOTLPMetricExporter()
		if err != nil {
			return nil, err
		}
		readerOpts = append(readerOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
	}

	meterProvider := sdkmetric.NewMeterProvider(readerOpts...)
	provider := &Provider{meterProvider: meterProvider, registry: registry}
	gen := generation.Add(1)
	current.Store(&meterState{
		gen:      gen,
		meter:    meterProvider.Meter(instrumentationScope),
		provider: provider,
	})
	return provider, nil
}

// Handler returns the Prometheus scrape handler for the process registry. If
// Setup has not run (tests and minimal tools), it installs the default
// Prometheus-only pipeline so the endpoint still works.
func Handler() http.Handler {
	st := current.Load()
	if st == nil {
		if _, err := Setup(Options{}); err != nil {
			slog.Error("metrics: initialize default registry", slog.Any("error", err))
			return promhttp.HandlerFor(prometheus.NewRegistry(), promhttp.HandlerOpts{})
		}
		st = current.Load()
	}
	return promhttp.HandlerFor(st.provider.registry, promhttp.HandlerOpts{})
}

// Registry returns the Prometheus registry behind /metrics. Callers see the
// translated (Prometheus) metric and label names.
func Registry() *prometheus.Registry {
	Handler() // install the default pipeline when Setup has not run
	st := current.Load()
	if st == nil {
		return prometheus.NewRegistry()
	}
	return st.provider.registry
}

// SnapshotValue returns the current value of a counter or gauge in the
// Prometheus registry. labels are key/value pairs. It reports false when the
// series does not exist yet.
func SnapshotValue(name string, labels ...string) (float64, bool) {
	if len(labels)%2 != 0 {
		return 0, false
	}
	families, err := Registry().Gather()
	if err != nil {
		return 0, false
	}
	want := make(map[string]string, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := make(map[string]string, len(want))
			for key, value := range want {
				matched[key] = value
			}
			for _, pair := range metric.GetLabel() {
				if value, ok := matched[pair.GetName()]; ok && value == pair.GetValue() {
					delete(matched, pair.GetName())
				}
			}
			if len(matched) != 0 {
				continue
			}
			switch {
			case metric.GetCounter() != nil:
				return metric.GetCounter().GetValue(), true
			case metric.GetGauge() != nil:
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// Shutdown flushes and stops the readers. It is idempotent and nil-safe.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		p.shutdownErr = p.meterProvider.Shutdown(ctx)
	})
	return p.shutdownErr
}

func buildResource(version string) (*resource.Resource, error) {
	defaults := resource.Default()
	version = strings.TrimSpace(version)
	if version == "" {
		return defaults, nil
	}
	merged, err := resource.Merge(defaults, resource.NewSchemaless(semconv.ServiceVersion(version)))
	if err != nil {
		return nil, fmt.Errorf("metrics: merge resource: %w", err)
	}
	return merged, nil
}

// newOTLPMetricExporter selects the OTLP transport from the standard
// OTEL_EXPORTER_OTLP_METRICS_PROTOCOL / OTEL_EXPORTER_OTLP_PROTOCOL variables
// (default http/protobuf). Endpoint, TLS, headers, compression, and timeouts
// are left to the exporter's own environment handling.
func newOTLPMetricExporter() (sdkmetric.Exporter, error) {
	protocol, err := otlpProtocol("METRICS")
	if err != nil {
		return nil, err
	}
	switch protocol {
	case "http/protobuf":
		exporter, err := otlpmetrichttp.New(context.Background())
		if err != nil {
			return nil, fmt.Errorf("metrics: create OTLP/HTTP exporter: %w", err)
		}
		return exporter, nil
	case "grpc":
		exporter, err := otlpmetricgrpc.New(context.Background())
		if err != nil {
			return nil, fmt.Errorf("metrics: create OTLP/gRPC exporter: %w", err)
		}
		return exporter, nil
	default:
		return nil, fmt.Errorf("metrics: unsupported OTLP protocol %q", protocol)
	}
}

func otlpRequested(configured bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_METRICS_EXPORTER"))) {
	case "otlp":
		return true
	case "none":
		return false
	default:
		return configured
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
			return "", fmt.Errorf("metrics: %s must be http/protobuf or grpc (got %q)", key, value)
		}
	}
	return "http/protobuf", nil
}

// RecordHTTPServerRequest observes one completed HTTP server request on the
// OTel semantic-convention histogram. route must be the matched route
// template, never the raw path. errorType is set only for failed requests.
func RecordHTTPServerRequest(ctx context.Context, method, route string, status int, errorType string, seconds float64) {
	instrument := httpServerRequestDuration.instrument()
	if instrument == nil {
		return
	}
	attrs := []attribute.KeyValue{semconv.HTTPRequestMethodKey.String(method)}
	if route != "" {
		attrs = append(attrs, semconv.HTTPRouteKey.String(route))
	}
	attrs = append(attrs, semconv.HTTPResponseStatusCodeKey.Int(status))
	if errorType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType))
	}
	instrument.Record(ctx, seconds, otelmetric.WithAttributes(attrs...))
}

// --- Instrument plumbing -----------------------------------------------------
//
// Instruments are created lazily from the current meter state. That lets the
// process install its real provider in cmd/api after package init without
// rebinding every call site, and keeps a no-op path safe before Setup.

type counterEntry struct {
	gen  uint64
	inst otelmetric.Float64Counter
}

type histogramEntry struct {
	gen  uint64
	inst otelmetric.Float64Histogram
}

type gaugeEntry struct {
	gen  uint64
	inst otelmetric.Float64Gauge
}

func meterStateFor() *meterState { return current.Load() }

// Counter is a monotonic, unlabelled counter.
type Counter struct {
	name string
	desc string
	unit string
	ref  atomic.Pointer[counterEntry]
}

func newCounter(name, desc string) *Counter {
	return &Counter{name: name, desc: desc}
}

func (c *Counter) instrument() otelmetric.Float64Counter {
	st := meterStateFor()
	if st == nil {
		return nil
	}
	if entry := c.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	instrumentM.Lock()
	defer instrumentM.Unlock()
	if entry := c.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	inst, err := st.meter.Float64Counter(c.name, otelmetric.WithDescription(c.desc), otelmetric.WithUnit(c.unit))
	if err != nil {
		return nil
	}
	c.ref.Store(&counterEntry{gen: st.gen, inst: inst})
	return inst
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds v.
func (c *Counter) Add(v float64) {
	if inst := c.instrument(); inst != nil {
		inst.Add(context.Background(), v)
	}
}

// CounterHandle is one label-value combination of a CounterVec.
type CounterHandle struct {
	inst  otelmetric.Float64Counter
	attrs attribute.Set
}

// Inc adds one.
func (h CounterHandle) Inc() { h.Add(1) }

// Add adds v.
func (h CounterHandle) Add(v float64) {
	if h.inst != nil {
		h.inst.Add(context.Background(), v, otelmetric.WithAttributeSet(h.attrs))
	}
}

// CounterVec is a labelled monotonic counter.
type CounterVec struct {
	name string
	desc string
	keys []attribute.Key
	ref  atomic.Pointer[counterEntry]
}

func newCounterVec(name, desc string, keys ...string) *CounterVec {
	attributeKeys := make([]attribute.Key, len(keys))
	for i, key := range keys {
		attributeKeys[i] = attribute.Key(key)
	}
	return &CounterVec{name: name, desc: desc, keys: attributeKeys}
}

func (v *CounterVec) instrument() otelmetric.Float64Counter {
	st := meterStateFor()
	if st == nil {
		return nil
	}
	if entry := v.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	instrumentM.Lock()
	defer instrumentM.Unlock()
	if entry := v.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	inst, err := st.meter.Float64Counter(v.name, otelmetric.WithDescription(v.desc))
	if err != nil {
		return nil
	}
	v.ref.Store(&counterEntry{gen: st.gen, inst: inst})
	return inst
}

// WithLabelValues returns the handle for values, in the order declared by the
// metric. A length mismatch records nothing rather than panicking.
func (v *CounterVec) WithLabelValues(values ...string) CounterHandle {
	inst := v.instrument()
	if inst == nil || len(values) != len(v.keys) {
		return CounterHandle{}
	}
	attrs := make([]attribute.KeyValue, 0, len(values))
	for i, value := range values {
		if i >= len(v.keys) {
			return CounterHandle{}
		}
		attrs = append(attrs, v.keys[i].String(value))
	}
	return CounterHandle{inst: inst, attrs: attribute.NewSet(attrs...)}
}

// HistogramHandle is one label-value combination of a HistogramVec.
type HistogramHandle struct {
	inst  otelmetric.Float64Histogram
	attrs attribute.Set
}

// Observe records one measurement.
func (h HistogramHandle) Observe(v float64) {
	if h.inst != nil {
		h.inst.Record(context.Background(), v, otelmetric.WithAttributeSet(h.attrs))
	}
}

// HistogramVec is a labelled histogram.
type HistogramVec struct {
	name   string
	desc   string
	unit   string
	bounds []float64
	keys   []attribute.Key
	ref    atomic.Pointer[histogramEntry]
}

func newHistogramVec(name, desc, unit string, keys ...string) *HistogramVec {
	attributeKeys := make([]attribute.Key, len(keys))
	for i, key := range keys {
		attributeKeys[i] = attribute.Key(key)
	}
	return &HistogramVec{name: name, desc: desc, unit: unit, keys: attributeKeys}
}

func newBoundedHistogramVec(name, desc, unit string, bounds []float64, keys ...string) *HistogramVec {
	histogram := newHistogramVec(name, desc, unit, keys...)
	histogram.bounds = bounds
	return histogram
}

func (v *HistogramVec) instrument() otelmetric.Float64Histogram {
	st := meterStateFor()
	if st == nil {
		return nil
	}
	if entry := v.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	instrumentM.Lock()
	defer instrumentM.Unlock()
	if entry := v.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	opts := []otelmetric.Float64HistogramOption{
		otelmetric.WithDescription(v.desc),
		otelmetric.WithUnit(v.unit),
	}
	if len(v.bounds) > 0 {
		opts = append(opts, otelmetric.WithExplicitBucketBoundaries(v.bounds...))
	}
	inst, err := st.meter.Float64Histogram(v.name, opts...)
	if err != nil {
		return nil
	}
	v.ref.Store(&histogramEntry{gen: st.gen, inst: inst})
	return inst
}

// WithLabelValues returns the handle for values, in declaration order.
func (v *HistogramVec) WithLabelValues(values ...string) HistogramHandle {
	inst := v.instrument()
	if inst == nil || len(values) != len(v.keys) {
		return HistogramHandle{}
	}
	attrs := make([]attribute.KeyValue, 0, len(values))
	for i, value := range values {
		if i >= len(v.keys) {
			return HistogramHandle{}
		}
		attrs = append(attrs, v.keys[i].String(value))
	}
	return HistogramHandle{inst: inst, attrs: attribute.NewSet(attrs...)}
}

// Gauge is an unlabelled instantaneous value.
type Gauge struct {
	name string
	desc string
	unit string
	ref  atomic.Pointer[gaugeEntry]
}

func newGauge(name, desc, unit string) *Gauge {
	return &Gauge{name: name, desc: desc, unit: unit}
}

func (g *Gauge) instrument() otelmetric.Float64Gauge {
	st := meterStateFor()
	if st == nil {
		return nil
	}
	if entry := g.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	instrumentM.Lock()
	defer instrumentM.Unlock()
	if entry := g.ref.Load(); entry != nil && entry.gen == st.gen {
		return entry.inst
	}
	inst, err := st.meter.Float64Gauge(g.name, otelmetric.WithDescription(g.desc), otelmetric.WithUnit(g.unit))
	if err != nil {
		return nil
	}
	g.ref.Store(&gaugeEntry{gen: st.gen, inst: inst})
	return inst
}

// Set records v.
func (g *Gauge) Set(v float64) {
	if inst := g.instrument(); inst != nil {
		inst.Record(context.Background(), v)
	}
}

// --- Metric definitions ------------------------------------------------------

var (
	httpServerRequestDuration = newBoundedHistogramVec(
		"http.server.request.duration",
		"Duration of HTTP server requests.",
		"s",
		httpServerDurationBuckets,
	)

	RateLimitErrorsTotal = newCounterVec(
		"rate_limit.errors",
		"Total number of rate-limit counter failures; the request was allowed (fail-open), partitioned by tier.",
		"tier",
	)

	JobsClaimedTotal = newCounterVec(
		"jobs.claimed",
		"Total number of jobs claimed by workers, partitioned by job kind.",
		"kind",
	)
	JobsSucceededTotal = newCounterVec(
		"jobs.succeeded",
		"Total number of jobs whose successful outcome was recorded, partitioned by job kind.",
		"kind",
	)
	JobsRetriedTotal = newCounterVec(
		"jobs.retried",
		"Total number of failed jobs requeued for another attempt, partitioned by job kind.",
		"kind",
	)
	JobsDeadTotal = newCounterVec(
		"jobs.dead",
		"Total number of jobs dead-lettered by a handler failure after exhausting their attempts, partitioned by job kind.",
		"kind",
	)
	JobsLeaseLostTotal = newCounterVec(
		"jobs.lease_lost",
		"Total number of running jobs whose handler was canceled because the worker lost the lease, partitioned by job kind.",
		"kind",
	)
	JobsLeaseDeadTotal = newCounterVec(
		"jobs.lease_dead",
		"Total number of jobs dead-lettered by lease expiry on their final attempt, partitioned by job kind.",
		"kind",
	)
	JobsQueueLagSeconds = newHistogramVec(
		"jobs.queue_lag",
		"Seconds a job waited between becoming due and being claimed, partitioned by job kind.",
		"s",
		"kind",
	)
	JobsClaimErrorsTotal = newCounter(
		"jobs.claim_errors",
		"Total number of failed job claim attempts; the worker retries with backoff.",
	)
	JobsPurgedTotal = newCounter(
		"jobs.purged",
		"Total number of terminal (succeeded or dead) job rows deleted by retention.",
	)

	AuditWriteFailuresTotal = newCounter(
		"audit.write_failures",
		"Total number of audit events that could not be recorded.",
	)
	AuditIntentFailuresTotal = newCounter(
		"audit.intent_failures",
		"Total number of pre-write audit intent rows that could not be recorded.",
	)
	AuditCompletionFailuresTotal = newCounter(
		"audit.completion_failures",
		"Total number of audit intent rows that could not be completed.",
	)
	AuditTxMissingTotal = newCounter(
		"audit.tx_missing",
		"Total number of Tier 1 audit writes attempted without a database transaction.",
	)
	AuditDomainMissingTotal = newCounter(
		"audit.domain_missing",
		"Total number of guarded mutations that succeeded without a domain audit event.",
	)

	FeatureFlagEvaluationsTotal = newCounterVec(
		"feature_flag.evaluations",
		"Total number of feature flag evaluations, partitioned by key, precedence source, and result.",
		"key", "source", "enabled",
	)
	FeatureFlagSnapshotLoadsTotal = newCounterVec(
		"feature_flag.snapshot_loads",
		"Total number of database snapshot loads, partitioned by result (success or error).",
		"result",
	)
	FeatureFlagCacheHitsTotal = newCounter(
		"feature_flag.cache_hits",
		"Total number of feature flag evaluations served from a fresh in-process snapshot.",
	)
	FeatureFlagCacheMissesTotal = newCounter(
		"feature_flag.cache_misses",
		"Total number of feature flag evaluations that found the snapshot cold or expired.",
	)
	FeatureFlagSnapshotStaleTotal = newCounter(
		"feature_flag.snapshot_stale",
		"Total number of evaluations served from a stale snapshot while the database was unavailable.",
	)
	FeatureFlagSnapshotFallbackTotal = newCounter(
		"feature_flag.snapshot_fallback",
		"Total number of evaluations that fell back to environment/compiled defaults because no snapshot was usable.",
	)
	FeatureFlagSnapshotAgeSeconds = newGauge(
		"feature_flag.snapshot_age",
		"Age in seconds of the feature flag snapshot currently used for evaluation.",
		"s",
	)
	FeatureFlagInvalidationsTotal = newCounter(
		"feature_flag.invalidations",
		"Total number of local feature flag snapshot invalidations.",
	)

	EventsPublishedTotal = newCounterVec(
		"events.published",
		"Total number of events published, partitioned by event name and transport.",
		"event", "mode",
	)
	EventsHandledTotal = newCounterVec(
		"events.handled",
		"Total number of event handler invocations, partitioned by event and outcome.",
		"event", "outcome",
	)
	EventsEnqueueErrorsTotal = newCounterVec(
		"events.enqueue_errors",
		"Total number of durable event jobs that could not be enqueued.",
		"event",
	)

	SchedulerRunsTotal = newCounterVec(
		"scheduler.runs",
		"Total number of scheduler slots processed, partitioned by schedule and outcome.",
		"schedule", "outcome",
	)
	SchedulerEnqueueErrorsTotal = newCounterVec(
		"scheduler.enqueue_errors",
		"Total number of scheduled jobs that could not be enqueued.",
		"schedule",
	)
	SchedulerLeader = newGauge(
		"scheduler.leader",
		"1 when this process holds the scheduler leader lease, otherwise 0.",
		"",
	)
	SchedulerRunsPurgedTotal = newCounter(
		"scheduler.runs_purged",
		"Total number of scheduler run-ledger rows deleted by retention.",
	)
)
