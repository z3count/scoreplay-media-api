// Package sqs provides an SQS-backed implementation of port.JobQueue.
//
// This is a STUB showing the exact integration points for switching from
// PostgreSQL to AWS SQS. When traffic grows beyond what PG polling can
// handle (>1000 jobs/s), implement this adapter and set JOB_QUEUE_BACKEND=sqs.
//
// Architecture with SQS:
//
//	HTTP Handler → Enqueue() → SQS SendMessage
//	                              ↓
//	                         Lambda trigger (or ECS worker)
//	                              ↓
//	                         JobHandler.Execute()
//	                              ↓
//	                         Write result to DB
//
// Key differences from the PG backend:
//   - Dequeue is a NO-OP: Lambda is triggered by SQS, not by polling.
//   - No in-process worker needed: Lambda handles execution.
//   - Complete/Fail update the DB directly (for observability), but the
//     SQS message is acknowledged/NACKed by Lambda.
//   - Retry is handled by SQS redrive policy (DLQ), not by the application.
//
// To implement:
//  1. Add aws-sdk-go-v2 dependency
//  2. Fill in Enqueue (SQS SendMessage)
//  3. Deploy a Lambda that calls JobHandler.Execute()
//  4. Set JOB_QUEUE_BACKEND=sqs and configure SQS_QUEUE_URL
package sqs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
)

// Config holds SQS connection settings.
type Config struct {
	QueueURL string // SQS queue URL (e.g., https://sqs.eu-west-1.amazonaws.com/123456789/media-jobs)
	Region   string // AWS region
}

// JobQueue implements port.JobQueue using AWS SQS.
//
// This stub compiles and satisfies the interface, but all methods return
// errors indicating they are not yet implemented. Replace each method
// body with actual SQS calls when ready to migrate.
type JobQueue struct {
	config Config
}

// NewJobQueue creates a new SQS-backed job queue.
func NewJobQueue(cfg Config) *JobQueue {
	return &JobQueue{config: cfg}
}

// Enqueue sends a job message to the SQS queue.
//
// TODO: Implement with SQS SendMessage:
//
//	client := sqs.NewFromConfig(awsCfg)
//	client.SendMessage(ctx, &sqs.SendMessageInput{
//	    QueueUrl:    &q.config.QueueURL,
//	    MessageBody: aws.String(string(payload)),
//	    MessageAttributes: map[string]types.MessageAttributeValue{
//	        "JobType": {DataType: aws.String("String"), StringValue: aws.String(jobType)},
//	    },
//	})
func (q *JobQueue) Enqueue(_ context.Context, _ string, _ json.RawMessage) (uuid.UUID, error) {
	return uuid.Nil, fmt.Errorf("sqs.JobQueue.Enqueue: not implemented — set JOB_QUEUE_BACKEND=postgres or implement SQS SendMessage")
}

// Dequeue is a no-op for SQS. Lambda is triggered by SQS directly,
// so there is no need for polling. The in-process worker should NOT
// be started when using the SQS backend.
func (q *JobQueue) Dequeue(_ context.Context) (*domain.Job, error) {
	return nil, nil // No-op: Lambda handles this.
}

// Complete updates the job record in the database (for observability).
//
// TODO: Implement with a direct DB update or API call.
// The SQS message is acknowledged by Lambda returning successfully.
func (q *JobQueue) Complete(_ context.Context, _ uuid.UUID, _ json.RawMessage) error {
	return fmt.Errorf("sqs.JobQueue.Complete: not implemented")
}

// Fail records the failure in the database.
//
// TODO: Implement with a direct DB update or API call.
// SQS retry is handled by the redrive policy (maxReceiveCount → DLQ).
func (q *JobQueue) Fail(_ context.Context, _ uuid.UUID, _ string) error {
	return fmt.Errorf("sqs.JobQueue.Fail: not implemented")
}

// Cleanup is a no-op for SQS. SQS messages expire automatically
// based on the queue's MessageRetentionPeriod setting.
func (q *JobQueue) Cleanup(_ context.Context, _ int) (int64, error) {
	return 0, nil
}
