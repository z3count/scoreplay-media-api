package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds the Prometheus metrics collectors for the HTTP layer.
//
// Exposed metrics:
//
//   - http_requests_total (counter): Total number of HTTP requests,
//     labeled by method, route pattern, and status code.
//     Use for alerting on error rate spikes (5xx) and traffic anomalies.
//
//   - http_request_duration_seconds (histogram): Request latency distribution,
//     labeled by method, route pattern, and status. Including status prevents
//     fast 4xx errors from masking slow 5xx successes when computing p99.
//
//   - http_requests_in_flight (gauge): Number of requests currently being
//     processed. Use for autoscaling decisions and capacity planning.
//
//   - http_panics_recovered_total (counter): Panics caught by the Recovery
//     middleware. Should be ~0 in steady state.
//
//   - http_auth_failures_total (counter): API key auth rejections,
//     labeled by reason (missing|invalid). Useful for brute-force detection.
//
//   - http_rate_limit_rejections_total (counter): Requests rejected by the
//     per-IP rate limiter. A non-zero rate is a saturation signal — either
//     traffic genuinely exceeds capacity or limits are too tight.
//
// All metrics use the chi route pattern (e.g., "/api/v1/media/{id}") as the
// "route" label, not the raw URL path. This prevents high-cardinality label
// explosion from UUID-heavy paths like "/api/v1/media/abc-123-def".
type Metrics struct {
	requestsTotal           *prometheus.CounterVec
	requestDuration         *prometheus.HistogramVec
	requestsInFlight        prometheus.Gauge
	panicsRecovered         prometheus.Counter
	authFailures            *prometheus.CounterVec
	rateLimitRejections     prometheus.Counter
}

// latencyBuckets extends the default Prometheus buckets (which cap at 10s)
// out to a minute. Media uploads can routinely exceed 10s on large files,
// and without longer buckets every slow upload piles into +Inf — p99 then
// reports "10s" regardless of how bad the tail actually is.
var latencyBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
}

// Singleton: promauto registers with the global Prometheus registry, so
// creating multiple Metrics instances panics on duplicate registration.
// sync.Once ensures collectors are created exactly once, even when
// multiple test routers call NewMetrics() in the same process.
var (
	metricsOnce     sync.Once
	metricsInstance *Metrics
)

// NewMetrics returns the singleton Prometheus metrics collectors.
// Safe to call multiple times (e.g., in tests that create multiple routers,
// or from non-HTTP middlewares that want to bump error-class counters).
func NewMetrics() *Metrics {
	metricsOnce.Do(func() {
		metricsInstance = &Metrics{
			requestsTotal: promauto.NewCounterVec(
				prometheus.CounterOpts{
					Name: "http_requests_total",
					Help: "Total number of HTTP requests by method, route, and status code.",
				},
				[]string{"method", "route", "status"},
			),
			requestDuration: promauto.NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "http_request_duration_seconds",
					Help:    "HTTP request latency distribution in seconds, labeled by status to separate error from success latencies.",
					Buckets: latencyBuckets,
				},
				[]string{"method", "route", "status"},
			),
			requestsInFlight: promauto.NewGauge(
				prometheus.GaugeOpts{
					Name: "http_requests_in_flight",
					Help: "Number of HTTP requests currently being processed.",
				},
			),
			panicsRecovered: promauto.NewCounter(
				prometheus.CounterOpts{
					Name: "http_panics_recovered_total",
					Help: "Number of panics caught by the Recovery middleware.",
				},
			),
			authFailures: promauto.NewCounterVec(
				prometheus.CounterOpts{
					Name: "http_auth_failures_total",
					Help: "Number of API key authentication failures, by reason.",
				},
				[]string{"reason"},
			),
			rateLimitRejections: promauto.NewCounter(
				prometheus.CounterOpts{
					Name: "http_rate_limit_rejections_total",
					Help: "Number of requests rejected by the per-IP rate limiter.",
				},
			),
		}
	})
	return metricsInstance
}

// IncPanicRecovered bumps the panic counter. Called from Recovery middleware.
func (m *Metrics) IncPanicRecovered() {
	m.panicsRecovered.Inc()
}

// IncAuthFailure bumps the auth-failure counter. reason should be a low-
// cardinality string like "missing" or "invalid".
func (m *Metrics) IncAuthFailure(reason string) {
	m.authFailures.WithLabelValues(reason).Inc()
}

// IncRateLimitRejection bumps the rate-limit rejection counter.
func (m *Metrics) IncRateLimitRejection() {
	m.rateLimitRejections.Inc()
}

// Handler returns a middleware that records Prometheus metrics for every request.
//
// It captures:
//   - Response status code (via statusWriter from logger.go)
//   - Request duration (from start to response completion)
//   - In-flight request count (incremented on entry, decremented on exit)
//
// Route patterns are resolved from chi's route context to avoid
// high-cardinality labels from dynamic path parameters.
func (m *Metrics) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.requestsInFlight.Inc()
		defer m.requestsInFlight.Dec()

		ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)

		// Use chi's route pattern for the label (e.g., "/api/v1/media/{id}")
		// to avoid cardinality explosion from UUID paths.
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unknown"
		}

		status := strconv.Itoa(ww.status)
		m.requestsTotal.WithLabelValues(r.Method, route, status).Inc()
		m.requestDuration.WithLabelValues(r.Method, route, status).Observe(time.Since(start).Seconds())
	})
}
