package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// JobQueue implements port.JobQueue using PostgreSQL.
//
// Dequeue uses SELECT FOR UPDATE SKIP LOCKED for concurrent, non-blocking
// job distribution across multiple workers. This is the same pattern used
// by Oban (Elixir), Graphile Worker (Node), and GoodJob (Rails).
//
// Failed jobs are re-scheduled with exponential backoff (30s, 60s, 120s...)
// up to max_attempts. After that, they are marked as permanently failed
// and can be inspected via SQL for debugging.
type JobQueue struct {
	db *sql.DB
}

// NewJobQueue creates a new PostgreSQL-backed job queue.
func NewJobQueue(db *sql.DB) *JobQueue {
	return &JobQueue{db: db}
}

// Enqueue inserts a new job into the queue, stamped with the producer's
// tenant id from context. The worker re-establishes that tenant in the
// handler's context at dequeue time so any downstream repo/storage call
// inherits the right scope.
func (q *JobQueue) Enqueue(ctx context.Context, jobType string, payload json.RawMessage) (uuid.UUID, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if payload == nil {
		payload = json.RawMessage("{}")
	}

	var id uuid.UUID
	err = q.db.QueryRowContext(ctx,
		`INSERT INTO jobs (tenant_id, type, payload) VALUES ($1, $2, $3) RETURNING id`,
		tenantID, jobType, payload,
	).Scan(&id)

	return id, err
}

// Dequeue atomically picks the next pending job and marks it as running.
//
// This is the *system-side* of the worker pipeline: the worker process
// is not a tenant, so the query is not tenant-scoped. The returned job
// carries its TenantID; the worker uses it to re-establish a
// tenant-scoped context before invoking the handler.
//
// The query uses SELECT FOR UPDATE SKIP LOCKED to ensure that multiple
// workers never pick the same job:
//  1. SELECT finds the oldest pending job that is due (scheduled_at <= now).
//  2. FOR UPDATE locks the row so no other transaction can pick it.
//  3. SKIP LOCKED makes other workers skip this row instead of waiting.
//  4. The outer UPDATE changes status to 'running' in the same statement.
//
// Returns nil, nil when no jobs are available (caller should sleep and retry).
func (q *JobQueue) Dequeue(ctx context.Context) (*domain.Job, error) {
	var job domain.Job
	var payload, result []byte
	var startedAt, completedAt sql.NullTime

	err := q.db.QueryRowContext(ctx, `
		UPDATE jobs SET status = 'running', started_at = now(), attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending' AND scheduled_at <= now()
			ORDER BY scheduled_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, tenant_id, type, payload, status, result, error, attempts, max_attempts,
		          scheduled_at, started_at, completed_at, created_at
	`).Scan(
		&job.ID, &job.TenantID, &job.Type, &payload, &job.Status, &result, &job.Error,
		&job.Attempts, &job.MaxAttempts, &job.ScheduledAt, &startedAt,
		&completedAt, &job.CreatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	job.Payload = payload
	job.Result = result
	if startedAt.Valid {
		job.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.Time
	}

	return &job, nil
}

// Complete marks a job as successfully completed with the given result.
func (q *JobQueue) Complete(ctx context.Context, id uuid.UUID, result json.RawMessage) error {
	_, err := q.db.ExecContext(ctx,
		`UPDATE jobs SET status = 'completed', result = $2, completed_at = now()
		 WHERE id = $1`,
		id, result,
	)
	return err
}

// Fail handles job failure with automatic retry logic.
//
// If the job has remaining attempts, it is re-scheduled with exponential
// backoff: 30s × 2^(attempt-1). Example for max_attempts=3:
//   - Attempt 1 fails → retry in 30s
//   - Attempt 2 fails → retry in 60s
//   - Attempt 3 fails → permanently marked as 'failed' (dead letter)
func (q *JobQueue) Fail(ctx context.Context, id uuid.UUID, errMsg string) error {
	_, err := q.db.ExecContext(ctx, `
		UPDATE jobs SET
			error = $2,
			status = CASE
				WHEN attempts >= max_attempts THEN 'failed'
				ELSE 'pending'
			END,
			scheduled_at = CASE
				WHEN attempts >= max_attempts THEN scheduled_at
				ELSE now() + (30 * power(2, attempts - 1)) * INTERVAL '1 second'
			END,
			completed_at = CASE
				WHEN attempts >= max_attempts THEN now()
				ELSE NULL
			END
		WHERE id = $1`,
		id, errMsg,
	)
	return err
}

// QueueStats is a point-in-time snapshot of queue depth used for saturation
// metrics. OldestPendingAge is zero when the queue is empty.
type QueueStats struct {
	Pending          int64
	Running          int64
	OldestPendingAge time.Duration
}

// Stats returns a snapshot of pending/running job counts and the age of the
// oldest pending job. This is the primary saturation signal for the worker
// pipeline: a growing Pending count or OldestPendingAge means jobs are
// arriving faster than the pool can process them.
//
// The query is a single round-trip with conditional aggregation to keep it
// cheap enough to sample every few seconds.
func (q *JobQueue) Stats(ctx context.Context) (QueueStats, error) {
	var s QueueStats
	var oldestSeconds sql.NullFloat64

	err := q.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'pending' AND scheduled_at <= now()),
			COUNT(*) FILTER (WHERE status = 'running'),
			EXTRACT(EPOCH FROM (now() - MIN(scheduled_at) FILTER (WHERE status = 'pending' AND scheduled_at <= now())))
		FROM jobs
	`).Scan(&s.Pending, &s.Running, &oldestSeconds)
	if err != nil {
		return QueueStats{}, err
	}
	if oldestSeconds.Valid && oldestSeconds.Float64 > 0 {
		s.OldestPendingAge = time.Duration(oldestSeconds.Float64 * float64(time.Second))
	}
	return s, nil
}

// Cleanup removes completed and failed jobs older than the given number of days.
// Returns the number of rows deleted. Should be called periodically (e.g., daily).
func (q *JobQueue) Cleanup(ctx context.Context, olderThanDays int) (int64, error) {
	result, err := q.db.ExecContext(ctx,
		`DELETE FROM jobs
		 WHERE status IN ('completed', 'failed')
		   AND completed_at < now() - ($1 || ' days')::INTERVAL`,
		olderThanDays,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// startCleanupLoop runs periodic cleanup of old jobs.
// Should be started as a goroutine.
func (q *JobQueue) StartCleanupLoop(ctx context.Context, interval time.Duration, retentionDays int) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.Cleanup(ctx, retentionDays) //nolint:errcheck
		}
	}
}
