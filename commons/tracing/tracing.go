// Package tracing wires the OpenTelemetry trace SDK for the dexPendulum Go
// services and provides thin helpers to instrument the boundaries where
// containers talk to each other (HTTP servers and clients).
//
// Init builds a TracerProvider that exports OTLP spans to the collector (Alloy,
// which forwards to Tempo) and returns a shutdown that flushes on exit. When
// OTEL_EXPORTER_OTLP_ENDPOINT is unset, Init is a no-op and tracing is simply
// disabled — tests and ad-hoc tooling run without a collector.
package tracing

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"commons/httproute"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Init installs the global TracerProvider and propagator for service.
//
// The OTLP gRPC endpoint comes from OTEL_EXPORTER_OTLP_ENDPOINT (host:port or
// http://host:port). The sampling ratio comes from OTEL_TRACES_SAMPLER_ARG
// (default 1.0), wrapped in ParentBased so a sampled inbound request keeps its
// whole trace. The returned shutdown flushes buffered spans — defer it in main.
func Init(ctx context.Context, service string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return noop, nil
	}
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return noop, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			attribute.String("service.name", serviceName(service)),
			attribute.String("service.version", version()),
			attribute.String("deployment.environment", env()),
		),
	)
	if err != nil {
		res = resource.NewSchemaless(attribute.String("service.name", serviceName(service)))
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(samplerRatio()))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// Tracer returns a named tracer from the global provider. Safe to call before
// Init: it yields a no-op tracer until the provider is installed.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// WrapHandler turns each inbound HTTP request into a server span and propagates
// the incoming trace context. Pair it with RouteNamer (placed innermost, next
// to the mux) so spans are named by route pattern instead of a constant.
func WrapHandler(next http.Handler, service string) http.Handler {
	return otelhttp.NewHandler(next, service)
}

// RouteNamer renames the active server span to "METHOD /route/pattern" once the
// ServeMux has matched, and records it as the http.route attribute. It must wrap
// the mux directly (below any context-replacing middleware) so that r.Pattern —
// set during dispatch — is visible on the same request after ServeHTTP returns.
func RouteNamer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Pattern == "" {
			return
		}
		// Hand the pattern to outer observers (metrics.Middleware) that sit
		// above context-replacing middleware and so never see r.Pattern.
		httproute.Record(r)
		span := trace.SpanFromContext(r.Context())
		if span.SpanContext().IsValid() {
			span.SetName(r.Method + " " + r.Pattern)
			span.SetAttributes(attribute.String("http.route", r.Pattern))
		}
	})
}

// Transport wraps an HTTP RoundTripper so outbound requests start client spans
// and inject the W3C trace context — this is what links a caller's trace across
// the container boundary into the callee's server span. base may be nil.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// Client returns an *http.Client whose Transport is trace-instrumented. Use it
// for cross-service and external calls so their latency/errors show up as spans.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(nil)}
}

func serviceName(fallback string) string {
	if v := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); v != "" {
		return v
	}
	return fallback
}

func env() string {
	if v := strings.TrimSpace(os.Getenv("DEPLOY_ENV")); v != "" {
		return v
	}
	return "development"
}

func version() string {
	if v := strings.TrimSpace(os.Getenv("SERVICE_VERSION")); v != "" {
		return v
	}
	return "dev"
}

func samplerRatio() float64 {
	v := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	if v == "" {
		return 1.0
	}
	r, err := strconv.ParseFloat(v, 64)
	if err != nil || r < 0 {
		return 1.0
	}
	if r > 1 {
		return 1.0
	}
	return r
}
