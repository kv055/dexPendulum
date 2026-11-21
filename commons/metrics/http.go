// Package metrics provides shared Prometheus HTTP instrumentation for the
// dexPendulum Go services.
//
// Each service builds its registry via NewServiceRegistry, wraps its
// http.ServeMux with Middleware, and exposes /metrics with Handler.
package metrics

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"commons/httproute"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServiceRegistry holds a service-scoped Prometheus registry plus the HTTP
// instruments registered against it.
type ServiceRegistry struct {
	Registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	durations *prometheus.HistogramVec
}

// NewServiceRegistry creates a fresh registry seeded with Go runtime + process
// collectors and the standard HTTP RED instruments. The service label lets
// Prometheus distinguish series from different binaries scraped at the same
// time.
func NewServiceRegistry(service string) *ServiceRegistry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	labels := prometheus.Labels{"service": service}

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "http_requests_total",
			Help:        "Total HTTP requests handled, labeled by method, route pattern and status code.",
			ConstLabels: labels,
		},
		[]string{"method", "route", "status"},
	)
	durations := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "http_request_duration_seconds",
			Help:        "HTTP request duration in seconds, labeled by method and route pattern.",
			ConstLabels: labels,
			Buckets:     prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)
	reg.MustRegister(requests, durations)

	return &ServiceRegistry{Registry: reg, requests: requests, durations: durations}
}

// Handler returns an http.Handler that serves the registry's metrics.
func (s *ServiceRegistry) Handler() http.Handler {
	return promhttp.HandlerFor(s.Registry, promhttp.HandlerOpts{Registry: s.Registry})
}

// Middleware wraps an http.Handler with request counting + latency timing.
//
// The route label comes from http.Request.Pattern (Go 1.22+ ServeMux), not the
// raw URL path — this keeps label cardinality bounded when handlers use path
// parameters like "/api/trading/accounts/{id}".
//
// When context-replacing middleware (session loaders, auth gates) sits between
// this middleware and the mux, the mux sets Pattern on a request copy this
// layer never sees. httproute bridges that gap: tracing.RouteNamer (innermost,
// next to the mux) records the pattern into a slot carried by the context.
func (s *ServiceRegistry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		r, matched := httproute.Capture(r)
		next.ServeHTTP(rec, r)

		route := matched()
		if route == "" {
			route = "unmatched"
		}
		s.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.durations.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Hijack forwards to the underlying ResponseWriter's Hijacker so WebSocket and
// other connection-upgrade handlers keep working behind this middleware.
//
// Embedding http.ResponseWriter does not promote Hijack (the interface doesn't
// declare it), so without this method the wrapper silently fails the
// w.(http.Hijacker) assertion that gorilla/websocket's Upgrade relies on,
// turning every upgrade into a 500.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics: underlying ResponseWriter does not implement http.Hijacker")
	}
	return hj.Hijack()
}
