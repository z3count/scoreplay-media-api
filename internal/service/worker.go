package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/scoreplay/media-api/internal/port"
)

// Worker polls the job queue and dispatches jobs to registered handlers.
//
// It runs N concurrent goroutines (configurable), each polling the queue
// independently. The SKIP LOCKED pattern in the PostgreSQL adapter ensures
// no two workers pick the same job.
//
// The worker is only started when using the PostgreSQL backend. For SQS,
// Lambda handles job execution directly — no in-process worker needed.
//
// Lifecycle:
//  1. Worker.Run(ctx) starts N goroutines.
//  2. Each goroutine polls Dequeue() on the configured interval.
//  3. When a job is found, it dispatches to the handler for that job type.
//  4. On success → queue.Complete(). On failure → queue.Fail().
//  5. When ctx is cancelled (shutdown), all goroutines drain and return.
type Worker struct {
	queue       port.JobQueue
	handlers   map[string]port.JobHandler
	logger     *slog.Logger
	poll       time.Duration
	concurrency int
}

// NewWorker creates a background job worker.
//
// Parameters:
//   - queue: the job queue backend (PostgreSQL or SQS).
//   - handlers: map of job type → handler. Unknown types are marked as failed.
//   - logger: structured logger.
//   - pollInterval: how often each goroutine polls for new jobs.
//   - concurrency: number of concurrent worker goroutines.
func NewWorker(
	queue port.JobQueue,
	handlers map[string]port.JobHandler,
	logger *slog.Logger,
	pollInterval time.Duration,
	concurrency int,
) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Worker{
		queue:       queue,
		handlers:   handlers,
		logger:     logger,
		poll:       pollInterval,
		concurrency: concurrency,
	}
}

// Run starts the worker pool. Blocks until ctx is cancelled.
// Each worker goroutine polls independently and processes jobs sequentially.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("background worker started",
		"concurrency", w.concurrency,
		"poll_interval", w.poll.String(),
		"handlers", w.handlerTypes(),
	)

	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			w.loop(ctx, workerID)
		}(i)
	}

	wg.Wait()
	w.logger.Info("background worker stopped")
}

// loop is the main poll loop for a single worker goroutine.
func (w *Worker) loop(ctx context.Context, workerID int) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.processOne(ctx, workerID)
		}
	}
}

// processOne dequeues and processes a single job.
func (w *Worker) processOne(ctx context.Context, workerID int) {
	job, err := w.queue.Dequeue(ctx)
	if err != nil {
		w.logger.Error("dequeue failed", "worker", workerID, "error", err)
		return
	}
	if job == nil {
		return // No jobs available.
	}

	logger := w.logger.With(
		"worker", workerID,
		"job_id", job.ID.String(),
		"job_type", job.Type,
		"attempt", job.Attempts,
	)
	metrics := newWorkerMetrics()

	handler, ok := w.handlers[job.Type]
	if !ok {
		logger.Error("unknown job type")
		metrics.jobsProcessed.WithLabelValues(job.Type, "unknown_type").Inc()
		_ = w.queue.Fail(ctx, job.ID, "unknown job type: "+job.Type)
		return
	}

	logger.Info("processing job")
	start := time.Now()

	result, execErr := handler.Execute(ctx, job.Payload)
	metrics.jobDuration.WithLabelValues(job.Type).Observe(time.Since(start).Seconds())
	if execErr != nil {
		logger.Error("job failed", "error", execErr)
		metrics.jobsProcessed.WithLabelValues(job.Type, "failed").Inc()
		_ = w.queue.Fail(ctx, job.ID, execErr.Error())
		return
	}

	if err := w.queue.Complete(ctx, job.ID, result); err != nil {
		logger.Error("failed to mark job complete", "error", err)
		// Don't bump completed here — the underlying job state is ambiguous.
		return
	}

	metrics.jobsProcessed.WithLabelValues(job.Type, "completed").Inc()
	logger.Info("job completed")
}

// handlerTypes returns the registered handler type names for logging.
func (w *Worker) handlerTypes() []string {
	types := make([]string, 0, len(w.handlers))
	for t := range w.handlers {
		types = append(types, t)
	}
	return types
}
