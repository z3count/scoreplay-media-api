// Package port — jobqueue.go defines the interfaces for the background job system.
//
// The job queue is the core abstraction that allows swapping backends:
//   - PostgreSQL (default): in-process worker polls with SKIP LOCKED.
//   - SQS/Lambda (future): Enqueue sends to SQS, Lambda handles dequeue.
//
// To switch backends, implement JobQueue and change JOB_QUEUE_BACKEND in config.
// The Worker and JobHandler interfaces remain identical regardless of backend.
package port

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
)

// JobEnqueuer is the narrow producer-side view of the job queue.
//
// Services that need to fire-and-forget a background task (e.g., "generate
// thumbnail for this media") should accept a JobEnqueuer rather than the
// full JobQueue. This keeps the producer surface minimal and makes mocking
// in tests trivial.
//
// Both PostgreSQL and SQS JobQueue implementations satisfy this interface
// automatically — no wrapping needed.
type JobEnqueuer interface {
	// Enqueue creates a new job and returns its ID.
	Enqueue(ctx context.Context, jobType string, payload json.RawMessage) (uuid.UUID, error)
}

// JobQueue defines the contract for enqueuing and processing background jobs.
//
// This is the interface that changes between backends:
//   - PostgreSQL: Enqueue inserts a row, Dequeue uses SELECT FOR UPDATE SKIP LOCKED.
//   - SQS: Enqueue sends a message, Dequeue receives a message.
//   - Lambda: Enqueue sends to SQS, Dequeue is unused (Lambda is triggered by SQS).
//
// Implementations must be safe for concurrent use.
type JobQueue interface {
	JobEnqueuer

	// Dequeue atomically picks the next pending job and marks it as running.
	// Returns nil, nil when no jobs are available (not an error).
	// For SQS backends, this may be a no-op (Lambda handles dequeue).
	Dequeue(ctx context.Context) (*domain.Job, error)

	// Complete marks a job as successfully completed with the given result.
	Complete(ctx context.Context, id uuid.UUID, result json.RawMessage) error

	// Fail marks a job as failed. If attempts < max_attempts, the job is
	// re-scheduled for retry with exponential backoff. Otherwise, it is
	// permanently marked as failed (dead letter).
	Fail(ctx context.Context, id uuid.UUID, errMsg string) error

	// Cleanup removes completed and failed jobs older than the given number
	// of days. Returns the number of rows deleted.
	Cleanup(ctx context.Context, olderThanDays int) (int64, error)
}

// JobHandler processes a specific type of background job.
//
// Each job type (e.g., "thumbnail", "transcode") has its own handler.
// Handlers receive their dependencies via struct injection (constructor),
// NOT via context values.
//
// Example implementation:
//
//	type ThumbnailHandler struct {
//	    mediaRepo port.MediaRepository
//	    storage   port.FileStorage
//	}
//
//	func (h *ThumbnailHandler) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
//	    var p struct{ MediaID string `json:"media_id"` }
//	    json.Unmarshal(payload, &p)
//	    // ... generate thumbnail using h.storage ...
//	    return json.Marshal(map[string]string{"path": thumbnailPath})
//	}
type JobHandler interface {
	// Execute processes the job payload and returns a result.
	// The context carries the request deadline and cancellation signal.
	// Dependencies (DB, storage, etc.) should be injected via the struct,
	// not passed through the context.
	Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)
}
