package postgres

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// queueMetrics exposes job-queue depth as Prometheus gauges. These are the
// primary saturation signal for the async pipeline:
//
//   - job_queue_pending: due pending jobs. A sustained climb means jobs
//     arrive faster than the worker pool processes them.
//   - job_queue_running: jobs currently in-flight (status='running').
//   - job_queue_oldest_pending_age_seconds: age of the oldest due pending
//     job, i.e. worst-case wait time for an enqueued unit of work.
//
// Gauges are owned by the Postgres adapter because they are derived from
// the `jobs` table. An SQS backend would expose equivalent signals through
// CloudWatch ApproximateNumberOfMessagesVisible / ApproximateAgeOfOldestMessage.
type queueMetrics struct {
	pending          prometheus.Gauge
	running          prometheus.Gauge
	oldestPendingAge prometheus.Gauge
}

var (
	queueMetricsOnce     sync.Once
	queueMetricsInstance *queueMetrics
)

func newQueueMetrics() *queueMetrics {
	queueMetricsOnce.Do(func() {
		queueMetricsInstance = &queueMetrics{
			pending: promauto.NewGauge(prometheus.GaugeOpts{
				Name: "job_queue_pending",
				Help: "Number of due pending jobs in the queue.",
			}),
			running: promauto.NewGauge(prometheus.GaugeOpts{
				Name: "job_queue_running",
				Help: "Number of jobs currently in the running state.",
			}),
			oldestPendingAge: promauto.NewGauge(prometheus.GaugeOpts{
				Name: "job_queue_oldest_pending_age_seconds",
				Help: "Age of the oldest due pending job in seconds. 0 if queue is empty.",
			}),
		}
	})
	return queueMetricsInstance
}

// StartStatsLoop samples queue depth on a ticker and writes Prometheus gauges
// until ctx is cancelled. The interval should be small enough to catch
// backlog growth (every 10–30s is typical) but not so small that the
// aggregation query becomes a meaningful DB cost.
func (q *JobQueue) StartStatsLoop(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	m := newQueueMetrics()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sample := func() {
		stats, err := q.Stats(ctx)
		if err != nil {
			// Stats failure is non-fatal — log at debug to avoid noise during
			// transient DB hiccups. The gauges keep their last value, which
			// is more useful than alerting on the scrape itself.
			if logger != nil {
				logger.Debug("job queue stats sample failed", "error", err)
			}
			return
		}
		m.pending.Set(float64(stats.Pending))
		m.running.Set(float64(stats.Running))
		m.oldestPendingAge.Set(stats.OldestPendingAge.Seconds())
	}

	sample() // Prime gauges on startup so dashboards don't show "no data".
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}
