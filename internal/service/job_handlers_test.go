package service

import (
	"context"
	"encoding/json"
	"testing"
)

// TestNoopJobHandler_EmptyPayload documents the contract: an empty payload
// should round-trip to the canonical "{}" so downstream readers don't have
// to special-case nil RawMessage.
func TestNoopJobHandler_EmptyPayload(t *testing.T) {
	h := NewNoopJobHandler()
	result, err := h.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != "{}" {
		t.Fatalf("empty payload should normalize to {}; got %q", string(result))
	}
}

// TestNoopJobHandler_EchoesPayload verifies the smoke-test handler returns
// the payload unchanged. Real handlers will transform; this one is the
// degenerate case used to validate the worker pipeline end-to-end.
func TestNoopJobHandler_EchoesPayload(t *testing.T) {
	h := NewNoopJobHandler()
	in := json.RawMessage(`{"media_id":"abc-123","quality":"high"}`)

	out, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("noop handler should echo payload; want %q got %q", string(in), string(out))
	}
}

// TestJobTypeNoopConstant guards the producer/consumer contract — the
// string is wired into the handler registry in main.go and into the metric
// label `jobs_processed_total{type="noop"}`. Changing it silently would
// break dashboards and pre-existing enqueued jobs.
func TestJobTypeNoopConstant(t *testing.T) {
	if JobTypeNoop != "noop" {
		t.Fatalf("JobTypeNoop changed: got %q, want %q. Coordinate with dashboards / migrations.", JobTypeNoop, "noop")
	}
}
