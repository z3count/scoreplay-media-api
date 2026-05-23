package service

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// workerMetrics holds Prometheus collectors for the background job worker.
//
// Exposed metrics:
//
//   - jobs_processed_total{type,outcome}: counter incremented for every job
//     processed. outcome is one of "completed", "failed", "unknown_type".
//     Errors derive from outcome != "completed"; success rate is the ratio.
//
//   - job_duration_seconds{type}: histogram of handler execution time.
//     Buckets stretch to 10 minutes since jobs (thumbnails, transcodes)
//     can legitimately be slow.
//
// Queue-depth gauges (job_queue_pending etc.) live with the Postgres adapter
// since they are an adapter-specific saturation signal — an SQS backend
// would expose its depth via CloudWatch instead.
type workerMetrics struct {
	jobsProcessed *prometheus.CounterVec
	jobDuration   *prometheus.HistogramVec
}

var jobDurationBuckets = []float64{
	0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600,
}

var (
	workerMetricsOnce     sync.Once
	workerMetricsInstance *workerMetrics
)

func newWorkerMetrics() *workerMetrics {
	workerMetricsOnce.Do(func() {
		workerMetricsInstance = &workerMetrics{
			jobsProcessed: promauto.NewCounterVec(
				prometheus.CounterOpts{
					Name: "jobs_processed_total",
					Help: "Total number of background jobs processed, by type and outcome.",
				},
				[]string{"type", "outcome"},
			),
			jobDuration: promauto.NewHistogramVec(
				prometheus.HistogramOpts{
					Name:    "job_duration_seconds",
					Help:    "Background job handler execution time in seconds.",
					Buckets: jobDurationBuckets,
				},
				[]string{"type"},
			),
		}
	})
	return workerMetricsInstance
}
