package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestMetrics builds a Metrics value backed by a fresh registry so tests
// don't collide on promauto's global default registry. The collector names
// are prefixed to allow multiple parallel test fixtures.
func newTestMetrics(t *testing.T, prefix string) *Metrics {
	t.Helper()
	registry := prometheus.NewRegistry()

	m := &Metrics{
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: prefix + "_http_requests_total", Help: "Total requests."},
			[]string{"method", "route", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    prefix + "_http_request_duration_seconds",
				Help:    "Latency.",
				Buckets: latencyBuckets,
			},
			[]string{"method", "route", "status"},
		),
		requestsInFlight: prometheus.NewGauge(
			prometheus.GaugeOpts{Name: prefix + "_http_requests_in_flight", Help: "In flight."},
		),
		panicsRecovered: prometheus.NewCounter(
			prometheus.CounterOpts{Name: prefix + "_http_panics_recovered_total", Help: "Panics."},
		),
		authFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: prefix + "_http_auth_failures_total", Help: "Auth failures."},
			[]string{"reason"},
		),
		rateLimitRejections: prometheus.NewCounter(
			prometheus.CounterOpts{Name: prefix + "_http_rate_limit_rejections_total", Help: "Rate-limit rejections."},
		),
	}
	registry.MustRegister(
		m.requestsTotal, m.requestDuration, m.requestsInFlight,
		m.panicsRecovered, m.authFailures, m.rateLimitRejections,
	)
	return m
}

// TestMetrics_Handler verifies that the metrics middleware records request
// count, duration, and in-flight gauge for each HTTP request.
func TestMetrics_Handler(t *testing.T) {
	m := newTestMetrics(t, "test")

	// Build a chi router to get proper route context.
	r := chi.NewRouter()
	r.Use(m.Handler)
	r.Get("/api/v1/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Make a request.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify counter was incremented.
	count := testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/api/v1/test", "200"))
	if count != 1 {
		t.Fatalf("expected http_requests_total=1, got %f", count)
	}

	// Make a second request.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/test", nil)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	count = testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/api/v1/test", "200"))
	if count != 2 {
		t.Fatalf("expected http_requests_total=2, got %f", count)
	}

	// Verify in-flight gauge is back to 0 (request completed).
	inFlight := testutil.ToFloat64(m.requestsInFlight)
	if inFlight != 0 {
		t.Fatalf("expected in-flight=0 after request, got %f", inFlight)
	}

	// Verify histogram has observations.
	histCount := testutil.CollectAndCount(m.requestDuration)
	if histCount == 0 {
		t.Fatal("expected histogram observations, got 0")
	}
}

// TestMetrics_DifferentStatusCodes verifies that different status codes
// produce separate counter and histogram series.
func TestMetrics_DifferentStatusCodes(t *testing.T) {
	m := newTestMetrics(t, "test2")

	r := chi.NewRouter()
	r.Use(m.Handler)
	r.Get("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	r.Get("/notfound", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// 200 request.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ok", nil))
	// 404 request.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/notfound", nil))

	ok200 := testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/ok", "200"))
	ok404 := testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/notfound", "404"))

	if ok200 != 1 {
		t.Fatalf("expected 1 request with status 200, got %f", ok200)
	}
	if ok404 != 1 {
		t.Fatalf("expected 1 request with status 404, got %f", ok404)
	}

	// Histogram should also be partitioned by status — verify both series exist.
	if testutil.CollectAndCount(m.requestDuration) < 2 {
		t.Fatalf("expected duration histogram observations for both statuses")
	}
}

// TestMetrics_ErrorClassCounters verifies the panic, auth-failure, and
// rate-limit counters expose data through the helper methods.
func TestMetrics_ErrorClassCounters(t *testing.T) {
	m := newTestMetrics(t, "test3")

	m.IncPanicRecovered()
	m.IncPanicRecovered()
	m.IncAuthFailure("missing")
	m.IncAuthFailure("invalid")
	m.IncAuthFailure("invalid")
	m.IncRateLimitRejection()

	if got := testutil.ToFloat64(m.panicsRecovered); got != 2 {
		t.Fatalf("panicsRecovered: want 2, got %f", got)
	}
	if got := testutil.ToFloat64(m.authFailures.WithLabelValues("missing")); got != 1 {
		t.Fatalf("authFailures{missing}: want 1, got %f", got)
	}
	if got := testutil.ToFloat64(m.authFailures.WithLabelValues("invalid")); got != 2 {
		t.Fatalf("authFailures{invalid}: want 2, got %f", got)
	}
	if got := testutil.ToFloat64(m.rateLimitRejections); got != 1 {
		t.Fatalf("rateLimitRejections: want 1, got %f", got)
	}
}
