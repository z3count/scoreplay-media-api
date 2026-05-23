package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// JobStatus represents the lifecycle state of a background job.
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

// Job represents a background task to be executed asynchronously.
//
// Jobs are created by the API layer (e.g., after media upload) and processed
// by a worker pool. The job system is backend-agnostic: the same Job struct
// is used whether the queue is PostgreSQL, SQS, or any other implementation.
//
// Lifecycle:
//
//	pending → running → completed
//	                  → failed (retry if attempts < max_attempts)
//
// Key invariants:
//   - Type identifies the handler to dispatch to (e.g., "thumbnail", "transcode").
//   - Payload contains job-specific data as JSON (e.g., {"media_id": "..."}).
//   - Result contains handler output as JSON (e.g., {"thumbnail_path": "..."}).
//   - Attempts is incremented on each execution. When attempts >= MaxAttempts,
//     the job is marked as permanently failed (dead letter).
type Job struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenantId"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	Status      JobStatus       `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"maxAttempts"`
	ScheduledAt time.Time       `json:"scheduledAt"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
}
