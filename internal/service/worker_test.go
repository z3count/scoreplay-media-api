package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// --- In-memory mock queue for testing ---

type mockJobQueue struct {
	mu   sync.Mutex
	jobs []*domain.Job
}

func newMockJobQueue() *mockJobQueue {
	return &mockJobQueue{}
}

func (q *mockJobQueue) Enqueue(_ context.Context, jobType string, payload json.RawMessage) (uuid.UUID, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job := &domain.Job{
		ID:          uuid.New(),
		Type:        jobType,
		Payload:     payload,
		Status:      domain.JobStatusPending,
		Attempts:    0,
		MaxAttempts: 3,
		ScheduledAt: time.Now(),
		CreatedAt:   time.Now(),
	}
	q.jobs = append(q.jobs, job)
	return job.ID, nil
}

func (q *mockJobQueue) Dequeue(_ context.Context) (*domain.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.jobs {
		if job.Status == domain.JobStatusPending {
			job.Status = domain.JobStatusRunning
			job.Attempts++
			now := time.Now()
			job.StartedAt = &now
			return job, nil
		}
	}
	return nil, nil
}

func (q *mockJobQueue) Complete(_ context.Context, id uuid.UUID, result json.RawMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.jobs {
		if job.ID == id {
			job.Status = domain.JobStatusCompleted
			job.Result = result
			now := time.Now()
			job.CompletedAt = &now
			return nil
		}
	}
	return fmt.Errorf("job not found: %s", id)
}

func (q *mockJobQueue) Fail(_ context.Context, id uuid.UUID, errMsg string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.jobs {
		if job.ID == id {
			job.Error = errMsg
			if job.Attempts >= job.MaxAttempts {
				job.Status = domain.JobStatusFailed
			} else {
				job.Status = domain.JobStatusPending
			}
			return nil
		}
	}
	return fmt.Errorf("job not found: %s", id)
}

func (q *mockJobQueue) Cleanup(_ context.Context, _ int) (int64, error) {
	return 0, nil
}

func (q *mockJobQueue) getJob(id uuid.UUID) *domain.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, job := range q.jobs {
		if job.ID == id {
			return job
		}
	}
	return nil
}

// --- Mock handlers ---

type successHandler struct{}

func (h *successHandler) Execute(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"status": "done"})
}

type failHandler struct{}

func (h *failHandler) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("processing failed")
}

// --- Tests ---

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWorker_ProcessesJobSuccessfully(t *testing.T) {
	queue := newMockJobQueue()
	handlers := map[string]port.JobHandler{"test": &successHandler{}}
	worker := NewWorker(queue, handlers, testLogger(), 50*time.Millisecond, 1)

	jobID, _ := queue.Enqueue(context.Background(), "test", json.RawMessage(`{"key":"value"}`))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go worker.Run(ctx)

	// Wait for processing.
	time.Sleep(200 * time.Millisecond)

	job := queue.getJob(jobID)
	if job.Status != domain.JobStatusCompleted {
		t.Errorf("expected completed, got %s", job.Status)
	}
	if job.Result == nil {
		t.Error("expected result, got nil")
	}
}

func TestWorker_FailsJobOnError(t *testing.T) {
	queue := newMockJobQueue()
	handlers := map[string]port.JobHandler{"fail-test": &failHandler{}}
	worker := NewWorker(queue, handlers, testLogger(), 50*time.Millisecond, 1)

	jobID, _ := queue.Enqueue(context.Background(), "fail-test", json.RawMessage(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go worker.Run(ctx)

	time.Sleep(400 * time.Millisecond)

	job := queue.getJob(jobID)
	if job.Error != "processing failed" {
		t.Errorf("expected error message, got %q", job.Error)
	}
	// With max_attempts=3 and fast polling, all retries complete.
	// The job ends as permanently failed after exhausting all attempts.
	if job.Status != domain.JobStatusFailed {
		t.Errorf("expected failed after retries exhausted, got %s", job.Status)
	}
	if job.Attempts < 3 {
		t.Errorf("expected at least 3 attempts, got %d", job.Attempts)
	}
}

func TestWorker_ExhaustsRetries(t *testing.T) {
	queue := newMockJobQueue()
	handlers := map[string]port.JobHandler{"exhaust": &failHandler{}}
	worker := NewWorker(queue, handlers, testLogger(), 30*time.Millisecond, 1)

	jobID, _ := queue.Enqueue(context.Background(), "exhaust", json.RawMessage(`{}`))
	// Set max_attempts to 1 so first failure is permanent.
	queue.getJob(jobID).MaxAttempts = 1

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go worker.Run(ctx)

	time.Sleep(200 * time.Millisecond)

	job := queue.getJob(jobID)
	if job.Status != domain.JobStatusFailed {
		t.Errorf("expected permanently failed, got %s", job.Status)
	}
}

func TestWorker_UnknownJobTypeMarkedFailed(t *testing.T) {
	queue := newMockJobQueue()
	handlers := map[string]port.JobHandler{} // No handlers registered.
	worker := NewWorker(queue, handlers, testLogger(), 50*time.Millisecond, 1)

	jobID, _ := queue.Enqueue(context.Background(), "unknown-type", json.RawMessage(`{}`))
	queue.getJob(jobID).MaxAttempts = 1

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go worker.Run(ctx)

	time.Sleep(200 * time.Millisecond)

	job := queue.getJob(jobID)
	if job.Status != domain.JobStatusFailed {
		t.Errorf("expected failed for unknown type, got %s", job.Status)
	}
	if job.Error == "" {
		t.Error("expected error message for unknown type")
	}
}

func TestWorker_StopsOnContextCancellation(t *testing.T) {
	queue := newMockJobQueue()
	worker := NewWorker(queue, nil, testLogger(), 50*time.Millisecond, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		worker.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// Worker stopped cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop within timeout")
	}
}

func TestWorker_ConcurrencyProcessesMultipleJobs(t *testing.T) {
	queue := newMockJobQueue()
	handlers := map[string]port.JobHandler{"concurrent": &successHandler{}}
	worker := NewWorker(queue, handlers, testLogger(), 30*time.Millisecond, 3)

	ids := make([]uuid.UUID, 5)
	for i := 0; i < 5; i++ {
		ids[i], _ = queue.Enqueue(context.Background(), "concurrent", json.RawMessage(`{}`))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go worker.Run(ctx)

	time.Sleep(400 * time.Millisecond)

	completed := 0
	for _, id := range ids {
		if queue.getJob(id).Status == domain.JobStatusCompleted {
			completed++
		}
	}
	if completed != 5 {
		t.Errorf("expected all 5 jobs completed, got %d", completed)
	}
}
