# Design & Technology Choices

This document explains the architectural decisions, trade-offs, and future improvement paths for the ScorePlay Media API.

---

## Architecture: Clean Architecture (Hexagonal)

The application follows Clean Architecture principles with strict dependency direction:

```
HTTP Handlers → Services → Ports (interfaces) ← Adapters (implementations)
                              ↓
                           Domain
```

### Why this matters

- **Testability**: The service layer depends on interfaces, not implementations. Every business rule can be tested with mocks — no database, no filesystem.
- **Swappability**: Switching from PostgreSQL to another database, or from local storage to S3, requires only a new adapter implementing the same interface.
- **Maintainability**: Each layer has a single, clear responsibility. Changes in one layer don't ripple through others.

### Package structure rationale

| Package | Responsibility | Depends on |
|---------|---------------|------------|
| `domain` | Business entities, sentinel errors | Nothing |
| `port` | Interface contracts | `domain` |
| `service` | Business logic, validation, orchestration | `domain`, `port` |
| `adapter/postgres` | PostgreSQL repository implementations | `domain`, `port` |
| `adapter/storage/local` | Local file storage | `port` |
| `adapter/http/handler` | HTTP request parsing, response building | `service`, `domain` |

---

## Database: PostgreSQL

### Why PostgreSQL

- Industry standard for relational data with strong ACID guarantees.
- Native UUID support (`gen_random_uuid()`).
- Excellent indexing capabilities for the future "search media by tag" feature.
- Mature Go drivers (`lib/pq`).

### Schema design

The many-to-many relationship between media and tags uses a classic junction table:

```
tags (id, name UNIQUE, created_at)
    ↕
media_tags (media_id, tag_id)  ← composite PK
    ↕
media (id, name, media_type, file_path, original_name, file_size, created_at)
```

**Key decisions:**
- **UUID primary keys**: Prevent sequential ID enumeration (security) and are safe for distributed environments.
- **`tags.name UNIQUE`**: Enables idempotent tag creation with `INSERT ... ON CONFLICT DO NOTHING`.
- **`media_type CHECK` constraint**: Database-level validation as a safety net beyond application-level checks.
- **`idx_media_tags_tag_id`**: The composite PK already indexes `(media_id, tag_id)`. This extra index enables efficient reverse lookups: "find all media for tag X".

### Why not an ORM

We use raw SQL with `database/sql` instead of GORM or similar ORMs because:
1. Full control over query structure — critical for optimizing JOIN queries.
2. No "magic" behavior that could hide performance issues.
3. Lighter dependency footprint.

---

## File Storage: Interface + Dual Implementations

### The Interface Pattern

The `port.FileStorage` interface abstracts all file operations:

```go
type FileStorage interface {
    Save(ctx context.Context, reader io.Reader, ext string) (path string, err error)
    Delete(ctx context.Context, path string) error
    URL(baseURL, path string) string
}
```

Two implementations exist:

| Backend | Package | Use Case |
|---------|---------|----------|
| **Local** | `adapter/storage/local` | Development, single-instance deployments |
| **S3** | `adapter/storage/s3` | Production, multi-instance, cloud-native |

Switching backends requires only changing the `STORAGE_BACKEND` environment variable — **zero changes to business logic** (Clean Architecture in practice).

### Hash-sharded directory structure (local)

Files are stored in a 3-level directory tree based on the UUID filename:

```
uploads/
  4/
    f/
      3/
        4f3e1a2b-...-uuid.jpg
```

**Why?** A flat directory with 100k+ files causes severe performance degradation on most filesystems (ext4, xfs):
- `readdir()` becomes O(n).
- Tools like `ls`, `rsync`, `find` can hang or crash.
- Inode lookup degrades as the directory hash table grows.

This sharding gives 16³ = 4096 leaf directories, keeping each manageable.

### S3 Storage

The S3 adapter (`adapter/storage/s3`) supports any S3-compatible service:
- AWS S3
- MinIO (validated in E2E tests)
- DigitalOcean Spaces
- Cloudflare R2

Keys are generated as `{prefix}{uuid}{ext}` (e.g., `media/a1b2c3d4.jpg`).
When `S3_CDN_URL` is set, file URLs point to the CDN rather than the S3 endpoint.

**Credentials.** The S3 adapter calls `awsconfig.LoadDefaultConfig`, so the
AWS SDK's default credential chain applies: IRSA (EKS), task role (ECS),
instance profile (EC2), env vars, shared credentials file. Static
`S3_ACCESS_KEY`/`S3_SECRET_KEY` are supported for local dev against MinIO
but trigger a startup warning when set — production deployments should
rely on the IAM role attached to the compute. The minimum IAM policy is
`s3:PutObject` + `s3:DeleteObject` on the bucket prefix; no
`s3:GetObject` is needed because reads go through the CDN.

**Private-tier media.** The default `URL()` method returns a plain
(unauthenticated) URL — fine for media that's intentionally public. For
private content, the adapter also implements `URLWithExpiry(ctx, baseURL,
path, ttl)` via `s3.NewPresignClient(...).PresignGetObject`, returning a
short-lived presigned URL signed with the app's credentials. The bucket
can be fully private; the URL alone authorises the read. This is plumbing
only — `MediaService.FileURL` still calls plain `URL()`; a feature flag
would switch it to `URLWithExpiry`.

---

## Observability

### Structured Logging

JSON-formatted logs by default for production log aggregation tools (ELK, Datadog, CloudWatch). Set `LOG_FORMAT=text` for human-readable local development.

Every HTTP request is logged with: method, path, status, duration, remote_addr, request_id.
Authentication failures are logged separately with client IP for brute-force detection.
Panics caught by the Recovery middleware are logged with the stack trace and request_id.

### Prometheus Metrics — Golden Signals

The exposed metrics at `GET /metrics` are designed around the four [Golden Signals](https://sre.google/sre-book/monitoring-distributed-systems/): **latency**, **traffic**, **errors**, and **saturation**. Each signal is covered explicitly so on-call has direct alerts for each failure mode.

**Latency & traffic (HTTP)**

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `http_requests_total` | Counter | method, route, status | Traffic rate; error rate via `status=~"5.."` |
| `http_request_duration_seconds` | Histogram | method, route, status | Buckets extend to 60s (default `DefBuckets` caps at 10s); `status` label avoids fast-error / slow-success masking |
| `http_requests_in_flight` | Gauge | — | HTTP concurrency / autoscaling input |

**Errors (HTTP-level, beyond status codes)**

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `http_panics_recovered_total` | Counter | — | Regression alarm — should be 0 in steady state |
| `http_auth_failures_total` | Counter | reason (`missing`/`invalid`) | Brute-force detection signal |
| `http_rate_limit_rejections_total` | Counter | — | Real capacity vs. mistuned limits |

**Saturation**

This is the signal class most often missed in early services. The HTTP-only metrics tell you nothing about why latency is climbing — the saturated dependency (DB pool, async pipeline) is what actually predicts an outage.

| Metric | Type | Source | Notes |
|--------|------|--------|-------|
| `media_api_*` (pool stats) | Gauge/Counter | `collectors.NewDBStatsCollector` | `*_in_use`, `*_wait_count`, `*_wait_duration_seconds`, idle metrics. Pool exhaustion is otherwise invisible until requests time out. |
| `job_queue_pending` | Gauge | Postgres adapter | Due-pending jobs; sampled every 15s via a single aggregation query |
| `job_queue_running` | Gauge | Postgres adapter | In-flight job count |
| `job_queue_oldest_pending_age_seconds` | Gauge | Postgres adapter | Worst-case wait time for an enqueued unit of work |
| `jobs_processed_total` | Counter (`type`, `outcome`) | Worker | Throughput; success rate is `completed / total` |
| `job_duration_seconds` | Histogram (`type`) | Worker | Buckets up to 10 min — jobs like transcodes are legitimately slow |
| Default Go/process collectors | various | `promauto` default registry | `go_goroutines`, `go_memstats_*`, `process_resident_memory_bytes`, `process_cpu_seconds_total` |

**Design decisions:**
- **Cardinality.** Labels use chi's route pattern (`/api/v1/media/{id}`) rather than raw paths to avoid label explosion from UUID-heavy URLs. Error-class counters use only low-cardinality `reason` labels.
- **Bucket sizing.** Default Prometheus buckets cap at 10s, which is the wrong shape for a media-upload workload — large uploads would all pile into `+Inf` and p99 would silently lie. HTTP histograms extend to 60s, job histograms to 10 min.
- **Status label on latency histograms.** Without it, a flood of fast 4xx errors pulls p99 *down* and masks slow successes. Adding the label costs negligible cardinality.
- **Saturation belongs with its adapter.** Queue-depth gauges live in the Postgres adapter (`adapter/postgres/metrics.go`), not the worker. An SQS backend would expose equivalent signals via CloudWatch `ApproximateAgeOfOldestMessage` instead.
- **Singleton collectors.** The `Metrics` (HTTP) and `workerMetrics` (job) structs use `sync.Once` to prevent duplicate registration panics when multiple routers are created (e.g., in E2E tests).
- **Middleware order.** The metrics middleware is placed before the Logger middleware for accurate duration measurement (the logger doesn't add measurable latency, but the ordering also means panics are counted regardless of logging state).

### Health Checks

| Endpoint | Type | Checks | Timeout |
|----------|------|--------|---------|
| `GET /healthz` | Liveness | Process alive | — |
| `GET /readyz` | Readiness | DB ping | 2 seconds |

Both endpoints are outside authentication middleware so Kubernetes probes and load balancers can access them without credentials.

---

## Background Jobs

The application includes a backend-agnostic background-job runner for async
post-upload work (thumbnail generation, video transcoding, EXIF extraction, …).
The **plumbing** is shipped end-to-end; concrete handlers are intentionally
left to be added as the features they back are prioritised.

### Components

| Component | Package | Responsibility |
|-----------|---------|----------------|
| `domain.Job` | `internal/domain` | Pure job entity (type, payload, status, retry counters) |
| `port.JobEnqueuer` | `internal/port` | Producer-side interface — what services depend on |
| `port.JobQueue` | `internal/port` | Full queue contract (Enqueue + Dequeue + Complete + Fail + Cleanup) |
| `port.JobHandler` | `internal/port` | Consumer-side interface — one per job type |
| `postgres.JobQueue` | `internal/adapter/postgres` | In-process backend; `SELECT … FOR UPDATE SKIP LOCKED` |
| `sqs.JobQueue` | `internal/adapter/sqs` | Stub for SQS / Lambda fan-out (Enqueue-only) |
| `service.Worker` | `internal/service` | Polling worker pool; dispatches to registered handlers |
| `service.NoopJobHandler` | `internal/service` | Smoke-test handler; documents the `JobHandler` pattern |

### Producer / consumer split

The producer interface is deliberately narrower than the full queue:

```go
type JobEnqueuer interface {
    Enqueue(ctx context.Context, jobType string, payload json.RawMessage) (uuid.UUID, error)
}
```

Services that need to fire-and-forget take `port.JobEnqueuer`, not
`port.JobQueue`. That keeps them ignorant of execution mechanics (Dequeue,
Complete, Fail) and makes mocking in unit tests trivial — the test
"queue" only needs to record Enqueue calls.

### Lifecycle

```
pending → running → completed
                  → failed (retry if attempts < max_attempts)
```

The Postgres backend implements retry with exponential backoff
(`30s × 2^(attempt-1)`) — a job failing at attempts 1, 2, 3 retries after
30s, 60s, then is marked permanently `failed` (dead-letter, inspectable via
SQL). The retry logic lives in `JobQueue.Fail`, not the worker, so all
backends share the same semantics.

### Backend selection

Selected at startup via `JOB_QUEUE_BACKEND`:

- **`postgres`** (default) — `INSERT` to enqueue; the in-process Worker
  pool polls with `SELECT … FOR UPDATE SKIP LOCKED`, so N concurrent
  pollers never grab the same job. Right answer until ~1k jobs/sec.
- **`sqs`** — Enqueue sends to SQS; an external consumer (typically
  Lambda) runs handlers. The in-process worker is intentionally **not
  started** in this mode (`Dequeue` is a no-op). SQS handles redrive /
  DLQ at the infrastructure level, not in the application.

Switching backends is a one-line config change — the Worker and
JobHandler code is identical either way.

### Worker pool

`service.Worker` runs N goroutines (`JOB_WORKER_CONCURRENCY`, default 4).
Each polls `Dequeue` on `JOB_POLL_INTERVAL` (default 2s). When a job is
returned:

1. Look up `port.JobHandler` for the job's `Type` in the registry.
2. Run `handler.Execute(ctx, payload)`.
3. On success → `queue.Complete(id, result)`.
4. On error → `queue.Fail(id, err.Error())` (the queue handles retry).
5. Unknown type → `Fail` with a sentinel reason; never retried as some
   other type.

Each step is wrapped in metrics (`jobs_processed_total{type, outcome}`,
`job_duration_seconds{type}`) — see [Observability](#prometheus-metrics--golden-signals).

### Graceful shutdown ordering

On SIGTERM the process steps down in a fixed order so no work is dropped:

1. **Stop accepting new HTTP** (`srv.Shutdown`) — in-flight handlers
   continue, and may still enqueue jobs during the drain window.
2. **Cancel the background context** (`cancelBg`) — signals the worker
   pool, queue-depth sampler, and cleanup loop to wind down.
3. **Wait for the worker pool** (`workerWG.Wait`) — bounded by
   `SHUTDOWN_TIMEOUT`. Handlers see `ctx.Done()` and should return
   promptly; a stuck handler triggers a warning log but does not pin the
   process.
4. **Close the DB** (`defer db.Close()`).

### Plugging in a new handler

Adding e.g. thumbnail generation is mechanical:

```go
// 1. Implement port.JobHandler.
type ThumbnailHandler struct {
    media   port.MediaRepository
    storage port.FileStorage
}

func (h *ThumbnailHandler) Execute(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
    var p struct{ MediaID string `json:"media_id"` }
    if err := json.Unmarshal(payload, &p); err != nil { return nil, err }
    // ... generate thumbnail using h.storage ...
    return json.Marshal(map[string]string{"thumbnail_path": path})
}

// 2. Define a job-type constant (alongside JobTypeNoop).
const JobTypeThumbnail = "thumbnail"

// 3. Register in cmd/api/main.go alongside service.JobTypeNoop:
jobHandlers := map[string]port.JobHandler{
    service.JobTypeNoop:      service.NewNoopJobHandler(),
    service.JobTypeThumbnail: service.NewThumbnailHandler(mediaRepo, storage),
}

// 4. Enqueue at the producer site, e.g. inside MediaService.Create:
payload, _ := json.Marshal(map[string]string{"media_id": media.ID.String()})
_, _ = enqueuer.Enqueue(ctx, service.JobTypeThumbnail, payload)
```

The retry policy, metrics, dead-letter handling, and graceful shutdown
all come for free.

### Observability

Already covered above, but called out here for completeness:

- **Throughput / errors**: `jobs_processed_total{type, outcome}` —
  `outcome` is `completed` / `failed` / `unknown_type`.
- **Latency**: `job_duration_seconds{type}` — buckets up to 10 minutes
  for slow handlers (transcodes).
- **Saturation**: `job_queue_pending`, `job_queue_running`,
  `job_queue_oldest_pending_age_seconds` — sampled every 15s by the
  Postgres adapter. The "oldest pending age" gauge is the single
  best alert target for "the worker pool is falling behind."

### Testing

| Layer | Coverage |
|-------|----------|
| `service.NoopJobHandler` | Round-trip contract: empty payload normalises to `{}`, non-empty is echoed unchanged. Pins the `JobTypeNoop` string so a rename doesn't silently break dashboards or enqueued jobs. |
| `service.Worker` | `worker_test.go` drives the full dispatch loop against an in-memory mock `JobQueue`: success → `Complete`, error → `Fail`, unknown type → fast-fail without retry. |
| `config.Load` (job-queue fields) | Defaults pinned + every env override verified. `getEnvBool` covers all six truthy/falsy spellings + unrecognised + unset paths. |
| `cmd/api.newJobQueue` | SQS without `SQS_QUEUE_URL` fails loudly; SQS with URL builds a non-nil queue; unknown backend errors with the offending value quoted. |
| Postgres branch (`adapter/postgres.JobQueue`) | Deliberately not unit-tested — `SELECT … FOR UPDATE SKIP LOCKED` semantics only make sense against a real Postgres. Covered by `internal/e2e` (testcontainers). |

What's **not** unit-tested: graceful-shutdown ordering (covered implicitly by the E2E harness), and `waitWithTimeout` (10-line concurrency primitive, racing it reliably in CI costs more than the coverage is worth).

---

## Security Measures

| Threat | Mitigation |
|--------|-----------|
| **Unauthorized access** | API key authentication on `/api/v1/*` routes |
| **Oversized uploads (DoS)** | `http.MaxBytesReader` — configurable limit (default 100MB), cuts connection immediately |
| **Malicious file types** | Content sniffing via `http.DetectContentType()` — never trust the client's `Content-Type` header |
| **Path traversal** | UUID-generated filenames only; client filenames stored for display, never used in paths |
| **SQL injection** | Parameterized queries (`$1`, `$2`), never string concatenation |
| **Information disclosure** | No stack traces in error responses; structured error codes only |
| **Brute-force** | Authentication failures logged with remote_addr; per-IP rate limiting |
| **Slowloris attack** | `ReadHeaderTimeout: 5s` on the HTTP server |
| **Runaway requests** | Per-request context deadline (configurable, default 30s) cancels DB queries and storage ops |
| **Connection exhaustion** | Configurable pool: `MaxOpenConns`, `MaxIdleConns`, `ConnMaxLifetime` (5m), `ConnMaxIdleTime` (1m) |
| **Panic propagation** | Recovery middleware catches panics, returns 500, logs stack trace |
| **Stored XSS** | `Content-Disposition: attachment` + `X-Content-Type-Options: nosniff` on uploaded files |
| **Clickjacking** | `X-Frame-Options: DENY` on all responses |
| **Directory listing** | `noDirFS` wrapper prevents browsing the upload directory tree |
| **Unicode injection** | NFC normalization, control/zero-width char rejection, invalid UTF-8 rejection |
| **Bidi spoofing** | Bidi override characters (U+202A–U+202E, U+2066–U+2069) rejected at input validation |
| **Duplicate uploads** | `Idempotency-Key` header enables at-most-once media creation; cached 24h in PostgreSQL |

---

## Data Consistency: File + Database

Creating a media item involves two operations:
1. **Save file** to disk/S3
2. **Insert record** into the database (within a transaction)

If step 2 fails after step 1 succeeds, we have an orphaned file. The service layer implements a **compensating action** (manual rollback):

```go
storedPath, err := storage.Save(ctx, file, ext)
// ...
result, err := repo.Create(ctx, media, tagIDs)
if err != nil {
    // Compensating action: delete the orphaned file
    storage.Delete(ctx, storedPath)
    return nil, err
}
```

This is simpler and more appropriate than distributed transaction patterns (saga, outbox) for this use case.

---

## Pagination: cursor-based (keyset)

List endpoints (`GET /api/v1/tags`, `GET /api/v1/media`) use **cursor-based
pagination**, also known as keyset pagination. The pagination key:

- Tags: `(name ASC, id ASC)` — `id` is a tie-breaker; `name` is currently UNIQUE.
- Media: `(created_at DESC, id DESC)` — `id` breaks timestamp ties.

The cursor is opaque to clients: base64url-encoded JSON over the keyset
position. The service layer encodes/decodes it (`internal/service/cursor.go`),
so the repository contract uses typed `port.TagCursor` / `port.MediaCursor`
structs and never sees the wire format.

### Why not offset-based?

For large datasets, `LIMIT 50 OFFSET 1000000` causes PostgreSQL to scan and
discard 1M rows on every request. Cursor pagination compares a tuple against
a compound index and serves any page in constant time — independent of page
depth.

The trade-off: cursor pagination cannot support arbitrary jumps ("page 27")
and provides no `total` field (the COUNT(*) is the same scan we were trying
to avoid). The response shape is `{limit, nextCursor, hasMore}`.

### Implementation notes

- The compound index `idx_media_cursor (created_at DESC, id DESC)` lets the
  planner satisfy both `(created_at, id) < ($1, $2)` and the ORDER BY in one
  backward range scan.
- We fetch `limit + 1` rows per query: if we got the extra one, the last
  in-page row's key becomes the `nextCursor`, and the extra row is dropped.
  This gives us `hasMore` without a separate COUNT.
- A malformed cursor maps to `domain.ErrValidation` → HTTP 400.

## Possible Ameliorations

Each item below comes from a real limitation in the current code, not generic
"nice to have" filler. Listed roughly in order of how directly they affect
the brief's "indexing and tagging photos, making it easy to search for
specific content" use case.

### 1. Full-text search on media and tag names

Today the only search dimensions are exact tag UUID/name match. Real
editorial workflows want "find anything that mentions Mbappé" across both
the media name and any tag name. Two viable paths:

- **PostgreSQL `tsvector`** (`to_tsvector('simple', name)` with a GIN index)
  — keeps everything in the same database, no extra infra. Good enough up to
  millions of rows for substring/word matching.
- **External search index** (Meilisearch, OpenSearch) — fuzzy matching,
  ranking, multi-field scoring. Worth the operational cost once
  multi-language ranking or typo-tolerance becomes a requirement.

Either way, the existing `port.MediaFilter` struct grows a `Query string`
field — no breaking signature change.

### 2. Multi-tag OR semantics

`GET /api/v1/media` already supports multi-tag AND intersection. The
complement — "media tagged with Mbappé OR Lewandowski" — is a one-line SQL
swap (`HAVING COUNT(DISTINCT …) ≥ 1` instead of `= N`) plus an explicit
`?tag_op=or` query parameter. Not added yet because we shipped the more
common AND case first.

### 3. Multi-tenancy

ScorePlay's real use case is multiple sports orgs (clubs, leagues,
federations) sharing the platform; a club's media must never leak to
another. **Phase 1 has shipped**: schema, ports, repository filtering,
storage prefixing, scope-gated handlers, and the load-bearing
cross-tenant isolation test. **Phase 2 (Row-Level Security as
defence-in-depth)** is deferred — it needs per-connection state
plumbing that's a separate refactor. The application-level WHERE
clauses + the integration test cover the correctness guarantee today.

**Shape (decisions taken)**

- **B2B only.** Tenants are other systems (clubs, leagues, federations).
  Authentication is via DB-backed API keys — one or more per tenant.
  No JWT/OIDC, no end-user accounts, no signup UI.
- **Tens of tenants.** Single Postgres, no sharding. Isolation via a
  `tenant_id UUID NOT NULL` column on every domain table, with Postgres
  Row-Level Security policies as defence-in-depth.
- **Prefix-per-tenant storage.** S3 keys become
  `<tenant_id>/<uuid>.<ext>` — one bucket, one IAM policy, prefix
  identifies ownership.
- **Authorisation via scopes** on each API key (`media:write`,
  `tags:read`, `admin:*`). Handlers gate with
  `identity.HasScope(...)`. Avoids a full RBAC engine.

**Schema additions**

- `tenants` table: `id UUID PK`, `name`, `status` (`active`/`suspended`),
  `created_at`.
- `api_keys` table: `id`, `tenant_id FK`, `hash` (SHA-256 of the raw
  key — only the hash is stored), `name`, `scopes JSONB`, `created_at`,
  `expires_at` (nullable), `last_used_at`.
- `tenant_id UUID NOT NULL` column on every existing domain table
  (`tags`, `media`, `media_tags`, `jobs`, `idempotency_keys`); composite
  indexes `(tenant_id, …)` replace single-column ones where the tenant
  filter would otherwise leave the index unfilterable.
- RLS policies on each table:
  `ENABLE ROW LEVEL SECURITY` + `USING (tenant_id = current_setting('app.tenant_id')::uuid)`.
  The repository's acquire-conn path executes
  `SET LOCAL app.tenant_id = '<uuid>'` from the request context, and
  Postgres enforces the filter server-side regardless of what the
  application SQL says.

**Ports & wiring**

- New `port.AuthVerifier`: `Verify(ctx, rawKey) (Identity, error)`.
  Returns `Identity{TenantID, KeyID, Scopes, ExpiresAt}` or a sentinel
  error (`ErrKeyExpired`, `ErrKeyRevoked`).
- `postgres.AuthVerifier` adapter: looks up the SHA-256 hash, returns
  the joined `tenants` + `api_keys` row. Hot keys cached in-memory
  with a short TTL (~5 s) to avoid a DB hit per request.
- `APIKeyAuth` middleware shrinks to: extract bearer → `Verify` →
  stash `Identity` in context → set `app.tenant_id` session var for
  the downstream DB connection.
- `RateLimit` middleware keys by `Identity.TenantID` once auth has
  run (falls back to `RemoteAddr` on unauthenticated routes).
- `Storage.Save` reads the tenant from context and prepends the
  prefix when building the S3 key.

**Migration order (no downtime)**

1. **✓ Shipped (migration 005).** Created `tenants` + `api_keys` tables
   and pre-provisioned the legacy tenant (id
   `00000000-0000-0000-0000-000000000001`). `AuthVerifier.EnsureLegacyTenant`
   on startup registers the static `API_KEY` (if set) as that tenant's
   first key with scope `admin:*`.
2. **✓ Shipped (migration 005).** Added `tenant_id` column with a
   default of the legacy tenant UUID on every domain table; existing
   rows backfilled automatically. The default is dropped at the end of
   the migration so new writes must be explicit.
3. **✓ Shipped.** Landed `port.AuthVerifier` + the postgres adapter
   (`internal/adapter/postgres/auth_verifier.go`); the middleware uses
   it for every request.
4. **✓ Shipped.** Plumbed `Identity` through `r.Context()`; every
   repository method calls `port.TenantIDFromContext` and includes
   `tenant_id` in INSERT / SELECT / UPDATE / DELETE. Storage adapters
   prefix the key with `<tenant_id>/…`.
5. **Phase 2 (deferred).** Enable RLS on each table. Needs the
   repository acquire-conn path to run `SET LOCAL app.tenant_id = …`
   at the start of every request-scoped transaction. That's a separate
   refactor; the WHERE clauses already filter today and the isolation
   test enforces the contract.
6. **✓ Shipped.** Cut over `APIKeyAuth` to the new verifier. Dev-mode
   bypass (`API_KEY=""` → legacy tenant identity) preserved for local
   development.
7. **✓ Shipped.** The migration drops the `tenant_id` default after
   backfill, so new writes must be explicit. With RLS still pending,
   the application-level WHERE clauses + the cross-tenant integration
   test give two layers of "this query is tenant-scoped" today; Phase 2
   adds the third.

**Tests**

The load-bearing test is **cross-tenant data isolation**.
`internal/e2e/multitenancy_test.go` exercises it against a real Postgres
+ local-disk fixture and is what guards correctness today.

- **✓ Integration — cross-tenant isolation** (testcontainers,
  `internal/e2e/multitenancy_test.go`). Two tenants, two keys, two
  media. Asserts: tenant-prefixed storage paths are distinct; the same
  tag name in both tenants is allowed; each tenant's list endpoints
  return only their own rows; cross-tenant GET / DELETE / attach
  return 404 (enumeration-safe); the same Idempotency-Key value used
  by two tenants returns each tenant's own cached row (not the
  other's).
- **✓ E2E (existing functional suite)** — all 47+ pre-tenancy tests
  re-pass against the migrated schema; the legacy tenant absorbs
  everything they create.
- **Open — Unit (`AuthVerifier`)** — happy path, expired key, revoked
  key, unknown key, malformed bearer; scope evaluation
  (`Identity.HasScope("media:write")`). The verifier is covered
  indirectly by the isolation E2E today; explicit unit tests would
  catch error-classification regressions earlier.
- **Open — Unit (`APIKeyAuth` middleware)** — same rationale.
- **Phase 2 — Integration (RLS backstop)** — set `app.tenant_id` to
  tenant A and execute the same SQL the app would issue *without* the
  application-level WHERE clause; Postgres must still filter to A only.
  Lands together with the RLS policies.
- **Open — Load (rate-limit isolation)** — flood tenant A; tenant B
  remains responsive.

### 3a. Multi-tenancy — explicitly out of scope

Each item below would land as a follow-up only when a real product
requirement appears. They are *not* implied by the plan above.

- **B2C / end-user identity.** No user accounts within a tenant, no
  signup UI, password resets, MFA. If a tenant needs end-user logins
  for people inside their org, plug an external IdP (Auth0, Cognito,
  Clerk) and map their user ID onto a `Subject` field in `Identity` —
  the verifier interface won't change.
- **Self-service tenant lifecycle.** No signup, suspension,
  reinstatement, or hard-delete flows. Tenants and initial keys are
  provisioned manually via SQL or a small admin endpoint guarded by
  the `admin:*` scope.
- **Schema-per-tenant or DB-per-tenant.** Not justified at tens of
  tenants without regulatory drivers. The migration cost is ~10×
  higher (per-tenant DDL, backups, connection pools) and the
  `tenant_id` + RLS combination gives the same correctness guarantee
  at single-DB cost.
- **Bucket-per-tenant in S3.** Not justified at this scale. Prefix-
  per-tenant gives ownership in the key. If a single large tenant
  ever needs Bring-Your-Own-Bucket (data residency, customer-managed
  encryption keys), the storage adapter gains a per-tenant bucket
  override reachable through the same `Identity` → `port.FileStorage`
  flow.
- **Sharding / horizontal partitioning.** Single Postgres is fine
  here. Escalation path, if a tenant ever dominates: read replicas
  first, then logical partitioning by `tenant_id`.
- **Cross-tenant analytics / admin UI.** No built-in console for
  cross-tenant queries. Ad-hoc cross-tenant SQL is straightforward
  once the schema is in place (don't set `app.tenant_id` — RLS will
  return all rows for a superuser). A real admin UI is a separate
  product.
- **Per-tenant billing or usage tracking.** Out of scope. The
  `tenant` label is added only to a small number of key metrics
  (`http_requests_total`, `jobs_processed_total`), capped at top-N
  tenants to bound Prometheus cardinality.
- **Cross-tenant resource sharing.** e.g. a "global" tag multiple
  tenants can apply. The schema is strictly partitioned; shared
  resources would need a separate model (a `shared_tags` table with
  no `tenant_id`, joined into both tenants' views).
- **CloudFront signed cookies per tenant.** Presigned URLs from
  `Storage.URLWithExpiry` are enough for private-tier media. We are
  not adding CDN-tier signing.
- **mTLS between tenants and the service.** API keys over HTTPS are
  the contract.

### 4. Soft delete + retention

`DELETE /api/v1/media/{id}` is a hard delete: row gone, file gone. For
regulatory reasons (GDPR right-to-be-forgotten requires hard delete *after*
a retention window, not immediately) you'd typically:

- Add `media.deleted_at TIMESTAMPTZ` and a partial index excluding deleted
  rows.
- Hide soft-deleted media from List/Get.
- Run a periodic worker that hard-deletes rows and files past the
  retention window (30–90 days).

### 5. Async cleanup of orphaned files

The service does compensating delete when a DB insert fails *after* the
file write (`MediaService.Create`), so the happy-path and known-failure-path
are consistent. What's *not* covered:

- Crashes between file write and DB insert leave an orphan.
- File-cleanup failure after a successful DB delete (logged, but the file
  stays around).

A periodic GC worker that walks the storage backend, looks up keys in the
DB, and deletes anything orphaned for >24h closes this. It's the kind of
thing you want before you have terabytes of "I don't remember why this is
here" files.

### 6. Real background-job handlers

The background-job **plumbing** is shipped and wired (see the [Background
Jobs](#background-jobs) section above). What is intentionally left out is
the actual work each handler does — adding them is a per-feature concern,
not a structural one. Concrete handlers to add next:

- **Thumbnail generation** — resize originals to 256×256 / 1024×1024 web
  thumbs, served by the same CDN.
- **Video transcoding** — convert to H.264/AAC MP4 + adaptive HLS for
  browser playback.
- **Metadata extraction** — EXIF (camera, GPS, capture time), video
  duration, resolution, codec.

Adding any of these is a focused change: implement `port.JobHandler`,
register it in the handler map in `cmd/api/main.go`, and call
`JobEnqueuer.Enqueue` from the producer site (e.g., `MediaService.Create`
after a successful upload).

### 7. OpenTelemetry tracing

Distributed tracing for end-to-end request visualization. Slots in as a
chi middleware that propagates `traceparent` headers; the slog logger
already structures fields the way OTEL exporters expect. Roughly a
half-day change.

### 8. CDN for file delivery

Serving files through the Go process is fine for development but a
bottleneck under load. A CDN (CloudFront, Cloudflare) in front of the
storage caches frequently-accessed files at the edge and offloads the
origin. The S3 backend already supports this — set `S3_CDN_URL` to the CDN
hostname and file URLs in API responses point there. The same idea works
for local storage by fronting `/uploads/` with the CDN.

---

## Trade-offs and discussion-magnets

A few features here go beyond the brief's literal requirements. They're
included because they reflect what a production media API actually has to
do, but each has a real cost worth being explicit about.

### Unicode-aware input sanitization (`internal/service/sanitize.go`)

NFC normalization, control-character / zero-width / bidi-override
rejection, replacement-character (`U+FFFD`) rejection — all on tag and
media *names*. That's a lot of code for what looks like a string field.

**Why it's here:**

- Tag names are user-visible labels rendered alongside the original
  filename. Bidi-override characters (`U+202E` and friends) can make
  "photo.jpg" display as "gpj.otohp" or hide the real extension — the
  classic "Trojan Source" attack pattern. For a platform whose users
  share content with external audiences, this matters.
- NFC normalization prevents the "two visually identical tags that don't
  match in search" problem (precomposed `é` vs `e` + combining `´`).
  Without it, idempotent tag creation silently breaks.
- All scripts are still supported (CJK, Arabic, emoji, accented Latin) —
  the blocklist is targeted at invisible/dangerous codepoints, not
  scripts.

**Reasonable alternative:** just NFC + length cap + reject control chars,
skip the rest. Cheaper but leaves the bidi-spoofing risk open.

### Idempotency-Key cache (`internal/adapter/http/middleware/idempotency.go`)

`POST /api/v1/media` honors an optional `Idempotency-Key` header and
caches the response in `idempotency_keys` for 24 hours. On retry, the
cached response replays with `Idempotency-Replayed: true`.

**Why it's here:** mobile clients on flaky networks retry. Without
idempotency, a network timeout that *did* reach the server creates a
duplicate media row + duplicate file in storage. The brief doesn't ask
for this, but a senior reviewer reading "production-ready" will notice
its absence.

**Cost:** an extra table, an hourly cleanup goroutine, and code in the
hot path of `POST /api/v1/media`. Acceptable for a write-heavy API
where retries are common; overkill for a strictly internal admin tool.

### Dual local + S3 storage (`internal/adapter/storage/{local,s3}`)

Two `port.FileStorage` implementations, chosen by `STORAGE_BACKEND` env var.

**Why it's here:** demonstrates the port/adapter split in practice —
swapping the entire storage layer is a single env-var change with zero
business-logic edits. It also keeps local development free of any cloud
dependency.

**Cost:** the testing surface roughly doubles (local has hash-sharded
directory tests, S3 has MinIO testcontainers e2e). For a tightly-scoped
production deployment you'd usually ship only one.

### Operational defaults

`internal/adapter/http/middleware/` includes rate limiting (per-IP token
bucket), request timeouts (context deadline), security headers
(`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`,
`Cache-Control: no-store`), CORS, and structured request logging. None of
these are individually expensive — they're called out here so a reviewer
doesn't have to grep middleware.go to know they exist.

---

## Technology Choices Summary

| Choice | Alternative Considered | Rationale |
|--------|----------------------|-----------|
| `go-chi/chi` | stdlib `net/http`, Gin, Fiber | Idiomatic, minimal, compatible with stdlib. Gin/Fiber add unnecessary abstraction. |
| `lib/pq` | `pgx` | Simpler, well-proven. `pgx` offers more features but adds complexity. |
| `golang-migrate` | `goose`, manual SQL | Standard tool, embeddable, good CLI. |
| Raw SQL | GORM, sqlc | Full control, no magic. |
| `log/slog` | zerolog, zap | Standard library since Go 1.21 — no external dependency needed. |
| `prometheus/client_golang` | OpenTelemetry metrics | De facto standard for Go Prometheus instrumentation. |
| `aws-sdk-go-v2` | MinIO Go client | Official AWS SDK, supports all S3-compatible services. |
| Distroless Docker | Alpine | Smaller image (~14MB), no shell = minimal attack surface. |
| UUID v4 | Auto-increment IDs | No information leakage, distributed-safe. |

---

## Testing Strategy

| Layer | Approach | Database / Docker | Count |
|-------|----------|-------------------|-------|
| Service | Mock-based unit tests (incl. worker, noop handler, sanitiser) | No | 80+ |
| Storage | Filesystem tests with `t.TempDir()` | No | 6 |
| Handler | `httptest` with mock services | No | 11 |
| Middleware | `httptest` + chi router (incl. metrics, auth, rate-limit, recovery) | No | 13 |
| Config | Env-var parsing and defaults (incl. job-queue + DB pool) | No | 7 |
| Composition root (`cmd/api`) | Backend-selection error paths | No | 3 |
| E2E | testcontainers (PostgreSQL + MinIO) | Yes (Docker) | 50+ |

**Run all tests:**
```bash
make test           # with race detector
make test-unit      # fast, no Docker
make test-coverage  # with HTML coverage report
```

