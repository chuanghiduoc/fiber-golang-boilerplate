package middleware

import (
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const httpInstrumentationName = "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"

// Tracing starts a server span for every request except excluded probe paths.
// It must run after RequestID so app.request_id is populated, and before
// Recovery/Logger/Metrics so the span covers the whole handler stack.
//
// Only low-cardinality, non-sensitive attributes are recorded: the matched
// route template (never the raw path or query), method, status, request ID,
// user ID, tenant ID, and an application error code. Error messages, request
// bodies, headers, and payloads are never attached.
//
// trustIncoming (TRACING_TRUST_INCOMING_TRACE) controls whether an incoming
// traceparent is honored. When the app trusts a proxy (APP_TRUST_PROXY), it
// is honored only from a peer in APP_TRUSTED_PROXIES.
func Tracing(tp trace.TracerProvider, excludePaths []string, trustIncoming bool, catalog *apperror.Catalog) fiber.Handler {
	excluded := make(map[string]struct{}, len(excludePaths))
	for _, path := range excludePaths {
		if path != "" {
			excluded[path] = struct{}{}
		}
	}
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	tracer := tp.Tracer(httpInstrumentationName)

	return func(c fiber.Ctx) error {
		if _, skip := excluded[c.Path()]; skip {
			return c.Next()
		}

		// W3C trace context from an upstream service or load balancer is
		// parsed by the OpenTelemetry propagator; malformed values are ignored.
		// An untrusted caller's traceparent is dropped: with a parent-based
		// sampler its sampled flag would otherwise force 100% sampling.
		ctx := c.Context()
		if trustIncoming && incomingTraceTrusted(c) {
			ctx = tracing.ExtractRemoteContext(ctx, c.Get(tracing.TraceParentHeader))
		}
		method := httpRequestMethod(c.Method())
		route := routeTemplate(c)
		ctx, span := tracer.Start(ctx, httpSpanName(method, route),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(serverSpanAttributes(c, method)...),
		)
		defer span.End()
		c.SetContext(ctx)

		err := c.Next()
		status := effectiveStatus(catalog, c, err)
		annotateServerSpan(c, span, err, status)
		markServerFailure(span, err, status)

		return err
	}
}

// annotateServerSpan records the bounded, non-sensitive request attributes
// known once the handler has run.
func annotateServerSpan(c fiber.Ctx, span trace.Span, err error, status int) {
	// Fiber resolves the route before middleware runs, but refresh after the
	// handler so mounted/sub-router templates are never missed.
	if route := routeTemplate(c); route != "" {
		span.SetName(httpSpanName(httpRequestMethod(c.Method()), route))
		span.SetAttributes(semconv.HTTPRouteKey.String(route))
	}
	span.SetAttributes(semconv.HTTPResponseStatusCodeKey.Int(status))

	if requestID := requestctx.RequestID(c.Context()); requestID != "" {
		span.SetAttributes(attribute.String("app.request_id", requestID))
	}
	if userID := requestctx.ActorID(c.Context()); userID != uuid.Nil {
		span.SetAttributes(semconv.EnduserIDKey.String(userID.String()))
	}
	if tenantID := requestTenantID(c); tenantID != uuid.Nil {
		span.SetAttributes(attribute.String("tenant.id", tenantID.String()))
	}

	var appErr *apperror.AppError
	if errors.As(err, &appErr) && appErr.Code != "" {
		span.SetAttributes(attribute.String("app.error_code", appErr.Code))
	}
}

// markServerFailure marks only 5xx spans failed, using a bounded error type
// rather than the error message. 4xx responses are expected client outcomes
// and stay Unset.
func markServerFailure(span trace.Span, err error, status int) {
	if status < fiber.StatusInternalServerError {
		return
	}
	if err != nil {
		tracing.MarkError(span, err)
		return
	}
	span.SetStatus(codes.Error, "")
	span.SetAttributes(semconv.ErrorTypeKey.String("http_" + strconv.Itoa(status)))
}

// serverSpanAttributes records the low-cardinality connection attributes the
// OTel HTTP semantic conventions ask for on a server span.
func serverSpanAttributes(c fiber.Ctx, method string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(method),
		semconv.URLSchemeKey.String(c.Scheme()),
		semconv.ClientAddressKey.String(c.IP()),
	}
	if version := networkProtocolVersion(c); version != "" {
		attrs = append(attrs, semconv.NetworkProtocolVersionKey.String(version))
	}
	if host := c.Hostname(); host != "" {
		attrs = append(attrs, semconv.ServerAddressKey.String(host))
	}
	if _, port := serverAddressPort(c); port > 0 {
		attrs = append(attrs, semconv.ServerPortKey.Int(port))
	}
	return attrs
}

// serverAddressPort returns the Host authority (without its port) and the port
// the client connected to. A Host header without a port implies 80 or 443.
func serverAddressPort(c fiber.Ctx) (host string, port int) {
	authority := strings.TrimSpace(c.Get(fiber.HeaderHost))
	host = c.Hostname()
	if _, rawPort, err := net.SplitHostPort(authority); err == nil {
		if parsed, err := strconv.Atoi(rawPort); err == nil {
			port = parsed
		}
	}
	if port == 0 {
		if c.Secure() {
			port = 443
		} else {
			port = 80
		}
	}
	return host, port
}

// networkProtocolVersion reports the HTTP version without its "HTTP/" prefix,
// normalizing HTTP/2.0 to the semconv value "2".
func networkProtocolVersion(c fiber.Ctx) string {
	protocol := strings.TrimSpace(c.Protocol())
	protocol = strings.TrimPrefix(protocol, "HTTP/")
	if protocol == "2.0" {
		return "2"
	}
	return protocol
}

// httpRequestMethod maps an unrecognized HTTP method to the semconv sentinel
// so a custom verb cannot create high-cardinality telemetry.
func httpRequestMethod(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case fiber.MethodGet, fiber.MethodHead, fiber.MethodPost, fiber.MethodPut,
		fiber.MethodDelete, fiber.MethodConnect, fiber.MethodOptions,
		fiber.MethodTrace, fiber.MethodPatch:
		return strings.ToUpper(strings.TrimSpace(method))
	default:
		return "_OTHER"
	}
}

// routeTemplate returns the registered route pattern. An unmatched request
// falls back to "/" in Fiber; it is reported as no route so the raw URL cannot
// leak into a span.
func routeTemplate(c fiber.Ctx) string {
	route := c.Route().Path
	if route == "" {
		return ""
	}
	if route == "/" && c.Path() != "/" {
		return ""
	}
	return route
}

// incomingTraceTrusted reports whether the direct peer may set the trace
// parent. Without proxy trust configured there is no peer allowlist, so the
// TRACING_TRUST_INCOMING_TRACE opt-in alone decides.
func incomingTraceTrusted(c fiber.Ctx) bool {
	if !c.App().Config().TrustProxy {
		return true
	}
	return c.IsProxyTrusted()
}

func httpSpanName(method, route string) string {
	if route == "" {
		return method
	}
	return method + " " + route
}

func requestTenantID(c fiber.Ctx) uuid.UUID {
	if id := tenanthttp.ID(c); id != uuid.Nil {
		return id
	}
	return tenant.IDFromContext(c.Context())
}
