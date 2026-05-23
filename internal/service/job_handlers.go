package service

import (
	"context"
	"encoding/json"

	"github.com/scoreplay/media-api/internal/port"
)

// Job type identifiers. Keep these as constants so producers (HTTP handlers,
// services) and consumers (handler registrations) refer to the same string.
// When adding a real handler, prefer adding the type name here.
const (
	// JobTypeNoop is a no-op job used as an end-to-end smoke test for the
	// worker pipeline. It accepts any payload and always succeeds.
	JobTypeNoop = "noop"
)

// NoopJobHandler is the canonical example handler. It does nothing and
// returns the payload unchanged.
//
// Two reasons it ships in the default handler registry:
//  1. It lets operators verify the full pipeline (enqueue → SKIP LOCKED
//     dequeue → handler dispatch → Complete + metrics) without writing any
//     real handler code.
//  2. It documents the JobHandler contract for future implementations. To
//     add a real handler (thumbnail, transcode, …):
//       - Define a struct with the dependencies it needs (e.g., MediaRepo,
//         FileStorage) injected through a constructor.
//       - Implement Execute(ctx, payload) → (result, error).
//       - Register it in the handler registry in cmd/api/main.go alongside
//         JobTypeNoop.
//       - Define a JobType* constant above and reference it from the
//         producer-side call to JobEnqueuer.Enqueue.
type NoopJobHandler struct{}

// NewNoopJobHandler returns a handler that succeeds without doing work.
func NewNoopJobHandler() *NoopJobHandler {
	return &NoopJobHandler{}
}

// Execute satisfies port.JobHandler. It returns the input payload as the
// result so callers can confirm the round-trip.
func (h *NoopJobHandler) Execute(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return json.RawMessage("{}"), nil
	}
	return payload, nil
}

// Compile-time check that NoopJobHandler satisfies the port.
var _ port.JobHandler = (*NoopJobHandler)(nil)
