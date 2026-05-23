package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/scoreplay/media-api/config"
)

// TestNewJobQueue_SQSRequiresURL verifies the SQS backend refuses to come up
// without SQS_QUEUE_URL. A silent fallback would be worse than failing fast:
// the worker would silently fail to enqueue anything in production.
func TestNewJobQueue_SQSRequiresURL(t *testing.T) {
	cfg := &config.Config{JobQueueBackend: "sqs"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := newJobQueue(context.Background(), cfg, nil, logger)
	if err == nil {
		t.Fatal("expected error when SQS_QUEUE_URL is empty")
	}
	if !strings.Contains(err.Error(), "SQS_QUEUE_URL") {
		t.Fatalf("error should mention SQS_QUEUE_URL, got: %v", err)
	}
}

// TestNewJobQueue_SQSSucceedsWithURL covers the happy SQS path. We pass nil
// for *sql.DB because the SQS branch never touches it — that's intentional
// (SQS replicas don't need DB access for queueing).
func TestNewJobQueue_SQSSucceedsWithURL(t *testing.T) {
	cfg := &config.Config{
		JobQueueBackend: "sqs",
		SQSQueueURL:     "https://sqs.eu-west-1.amazonaws.com/123/q",
		SQSRegion:       "eu-west-1",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	q, err := newJobQueue(context.Background(), cfg, nil, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q == nil {
		t.Fatal("queue should not be nil")
	}
}

// TestNewJobQueue_UnknownBackend guards the typo case. Defaulting silently
// would mask config errors; we want startup to fail loudly.
func TestNewJobQueue_UnknownBackend(t *testing.T) {
	cfg := &config.Config{JobQueueBackend: "redis"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := newJobQueue(context.Background(), cfg, nil, logger)
	if err == nil {
		t.Fatal("expected error for unknown backend")
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Fatalf("error should mention the offending value, got: %v", err)
	}
}
