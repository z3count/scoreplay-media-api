-- Background job queue table.
--
-- Used by the in-process worker to poll for pending jobs using the
-- SELECT FOR UPDATE SKIP LOCKED pattern. This enables multiple workers
-- to process jobs concurrently without duplicate execution.
--
-- Lifecycle: pending → running → completed | failed
-- Failed jobs with attempts < max_attempts are re-scheduled automatically.
CREATE TABLE IF NOT EXISTS jobs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    type         TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}',
    status       TEXT        NOT NULL DEFAULT 'pending',
    result       JSONB,
    error        TEXT,
    attempts     INT         NOT NULL DEFAULT 0,
    max_attempts INT         NOT NULL DEFAULT 3,
    scheduled_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index for efficient dequeue: only pending jobs are indexed.
-- On a table with 1M jobs and 50 pending, this index has 50 entries.
-- The dequeue query scans this index in O(1).
CREATE INDEX IF NOT EXISTS idx_jobs_dequeue
    ON jobs (scheduled_at)
    WHERE status = 'pending';

-- Index for cleanup queries (delete old completed/failed jobs).
CREATE INDEX IF NOT EXISTS idx_jobs_cleanup
    ON jobs (completed_at)
    WHERE status IN ('completed', 'failed');
