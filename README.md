# ScorePlay Media API

A production-ready REST API for managing media files (photos and videos) with tag-based organization, built in Go following Clean Architecture principles.

## Features

- **Media management**: Upload, list, retrieve, and delete photos and videos with metadata
- **Tag-based organization**: Create tags and associate them with media items (many-to-many). Upload-by-tag-name auto-creates missing tags so editors don't have to look up UUIDs.
- **Dual storage backends**: Local filesystem or S3-compatible storage (MinIO, AWS S3, DigitalOcean Spaces)
- **Paginated listing**: List media and tags with cursor-based (keyset) pagination — constant-time regardless of page depth
- **Tag filtering**: Filter media by tag UUID(s) or name(s) — repeat the parameter for AND intersection (`?tag_name=Mbappé&tag_name=Ligue+1`)
- **Content-type detection**: Server-side content sniffing — never trusts client-provided headers
- **Unicode input validation**: NFC normalization, zero-width/bidi character rejection, whitespace normalization — supports all scripts (CJK, Arabic, emoji…)
- **API key authentication**: Configurable API key for securing endpoints
- **Prometheus metrics**: Golden Signals (latency, traffic, errors, saturation) at `/metrics` — HTTP counters/histograms, DB pool stats, job-queue depth, panic/auth/rate-limit counters
- **Health checks**: Kubernetes-style `/healthz` (liveness) and `/readyz` (readiness) probes
- **Structured logging**: JSON by default for log aggregation (ELK, Datadog, CloudWatch)
- **Idempotent uploads**: `Idempotency-Key` header prevents duplicate media on retries
- **Rate limiting**: Per-IP token bucket (configurable, default 10 req/s, burst 30)
- **Security headers**: CORS, X-Content-Type-Options, X-Frame-Options, Content-Disposition
- **Background jobs**: Backend-agnostic job runner (Postgres `SKIP LOCKED` worker or SQS/Lambda fan-out) with retries, dead-letter, and saturation metrics — handler-registry-based, ready for thumbnail/transcode/etc. to plug in
- **Graceful shutdown**: Drains in-flight HTTP and background workers on SIGTERM
- **CI/CD**: GitHub Actions pipelines for lint, test, build, and Docker image validation

## API Documentation

The full API is described in an [OpenAPI 3.0 specification](openapi.yaml). You can visualize it with:

```bash
# Swagger UI (browser)
npx -y @redocly/cli preview openapi.yaml

# Or paste into https://editor.swagger.io
```

## Quick Start

### Prerequisites

- **Go** 1.25+ ([install](https://go.dev/doc/install))
- **Docker** and **Docker Compose** ([install](https://docs.docker.com/get-docker/))

### Run Locally

```bash
# 1. Start PostgreSQL
docker compose up -d

# 2. Run the application (migrations run automatically on startup)
make run

# Or without Make:
LOG_FORMAT=text go run ./cmd/api/
```

The server starts at `http://localhost:8080`.

### Run Tests

```bash
# Unit tests (no Docker required)
make test-unit

# All tests including E2E (requires Docker for testcontainers)
make test

# Coverage report
make test-coverage
```

### 30-second demo

Once the server is running, this minimal flow exercises every required
endpoint end-to-end. `API_KEY` is left empty by default in dev (auth
disabled); set it in `.env` to enable auth and pass `-H "X-API-Key: …"`
on each request.

```bash
# 1. Upload a media — tag names that don't exist yet are auto-created.
curl -s -X POST http://localhost:8080/api/v1/media \
  -F "name=Goal celebration" \
  -F "tag_names=Mbappé" \
  -F "tag_names=Ligue 1" \
  -F "file=@/path/to/photo.jpg" | jq .

# 2. List media filtered by tag name (works without knowing any UUIDs).
curl -s "http://localhost:8080/api/v1/media?tag_name=Ligue+1" | jq .

# 3. List the tags that now exist (Mbappé + Ligue 1 created in step 1).
curl -s "http://localhost:8080/api/v1/tags" | jq .

# 4. Explicit tag creation also works when you want a curated taxonomy.
curl -s -X POST http://localhost:8080/api/v1/tags \
  -H "Content-Type: application/json" \
  -d '{"name":"UEFA Champions League"}' | jq .
```

For a ready-to-run set of these requests in VS Code's REST Client / JetBrains
HTTP Client, see [`api.http`](api.http).

---

## API Reference

All API endpoints require an `X-API-Key` header (when `API_KEY` is configured).
Responses follow a consistent JSON envelope:

```json
// Success
{ "data": { ... } }

// Error
{ "error": { "code": "ERROR_CODE", "message": "description" } }
```

### Endpoints

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `GET` | `/healthz` | ❌ | Liveness probe — always 200 if process is alive |
| `GET` | `/readyz` | ❌ | Readiness probe — 200 if DB is reachable, 503 otherwise |
| `GET` | `/metrics` | ❌ | Prometheus metrics (scrape target) |
| `POST` | `/api/v1/tags` | ✅ | Create a tag (idempotent) |
| `GET` | `/api/v1/tags` | ✅ | List tags (paginated) |
| `PATCH` | `/api/v1/tags/{id}` | ✅ | Rename a tag |
| `DELETE` | `/api/v1/tags/{id}` | ✅ | Delete a tag (cascades to `media_tags`) |
| `POST` | `/api/v1/media` | ✅ | Upload a media file |
| `GET` | `/api/v1/media` | ✅ | List media (paginated, optional tag filter) |
| `GET` | `/api/v1/media/{id}` | ✅ | Get a single media item |
| `DELETE` | `/api/v1/media/{id}` | ✅ | Delete a media item (DB + file) |
| `POST` | `/api/v1/media/{id}/tags` | ✅ | Attach more tags (UUIDs and/or names) to an existing media |
| `DELETE` | `/api/v1/media/{id}/tags/{tag_id}` | ✅ | Unlink one tag from a media; tag itself stays |
| `GET` | `/uploads/*` | ❌ | Static file server (Content-Disposition: attachment) |

---

### Create a Tag

**`POST /api/v1/tags`**

Creates a new tag or returns the existing one if a tag with the same name already exists (idempotent).

```bash
curl -X POST http://localhost:8080/api/v1/tags \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{"name": "Mbappé"}'
```

**Response (201 Created / 200 OK):**
```json
{
  "data": {
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "name": "Mbappé",
    "createdAt": "2026-05-21T12:00:00Z"
  }
}
```

| Status | Meaning |
|--------|---------|
| 201 | Tag created |
| 200 | Tag already exists (returned existing) |
| 400 | Invalid input (empty name, too long) |

---

### Rename a Tag

**`PATCH /api/v1/tags/{id}`**

Updates the name of an existing tag. The new name goes through the same
sanitization pipeline as creation, and the `tags.name UNIQUE` constraint
guarantees no duplicates. The rename is reflected immediately in every media
item that references the tag.

```bash
curl -X PATCH http://localhost:8080/api/v1/tags/550e8400-... \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{"name": "Mbappé"}'
```

| Status | Meaning |
|--------|---------|
| 200 | Tag renamed (or no-op if name unchanged) |
| 400 | Invalid UUID, malformed JSON, or invalid name |
| 404 | No tag with that id |
| 409 | Another tag already uses the new name |
| 415 | Content-Type is not application/json |

---

### Delete a Tag

**`DELETE /api/v1/tags/{id}`**

Removes a tag and (via `ON DELETE CASCADE` on `media_tags`) all of its
media associations. Media items themselves are not deleted — they just lose
this tag.

```bash
curl -X DELETE http://localhost:8080/api/v1/tags/550e8400-... \
  -H "X-API-Key: your-api-key"
```

**Response:** `204 No Content` (empty body)

| Status | Meaning |
|--------|---------|
| 204 | Tag deleted |
| 400 | Invalid UUID format |
| 404 | No tag with that id |

---

### List Tags

**`GET /api/v1/tags?limit=10&cursor=<opaque>`**

Returns a cursor-paginated list of all tags, ordered alphabetically. Pass the
`nextCursor` from the previous response as `cursor` to fetch the next page.

| Param | Type | Default | Max | Description |
|-------|------|---------|-----|-------------|
| `limit` | int | 50 | 100 | Items per page |
| `cursor` | string | — | — | Opaque cursor from previous response (omit for first page) |

**Response (200 OK):**
```json
{
  "data": {
    "tags": [
      { "id": "...", "name": "Ligue 1", "createdAt": "..." },
      { "id": "...", "name": "Mbappé", "createdAt": "..." }
    ],
    "pagination": {
      "limit": 10,
      "nextCursor": "eyJuIjoiTWJhcHDDqSIsImkiOiIuLi4ifQ",
      "hasMore": true
    }
  }
}
```

When `hasMore` is `false`, `nextCursor` is omitted — there are no further pages.

---

### Upload a Media

**`POST /api/v1/media`** (multipart/form-data)

```bash
# Editor-friendly: pass tag names; the server auto-creates any that
# don't exist yet (idempotent CreateOrGet under the hood).
curl -X POST http://localhost:8080/api/v1/media \
  -H "X-API-Key: your-api-key" \
  -F "name=Goal celebration" \
  -F "tag_names=Mbappé" \
  -F "tag_names=Ligue 1" \
  -F "file=@photo.jpg"

# Or if you already know the tag UUIDs, pass them directly.
# tags and tag_names can also be mixed in the same request.
curl -X POST http://localhost:8080/api/v1/media \
  -H "X-API-Key: your-api-key" \
  -F "name=Goal celebration" \
  -F "tags=550e8400-e29b-41d4-a716-446655440000" \
  -F "file=@photo.jpg"
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Human-readable media name |
| `tags` | string[] (repeatable) | no | Tag UUIDs to attach (caller already knows them) |
| `tag_names` | string[] (repeatable) | no | Tag names — names that don't exist yet are created on-the-fly. Sanitized the same way as `POST /tags`. |
| `file` | binary | yes | Image or video file |

`tags` and `tag_names` are additive: a single request can use either, both, or neither. The combined set is deduped server-side and capped at 50.

**Supported file types:** JPEG, PNG, GIF, WebP, MP4, WebM, MOV, AVI, MKV

**Response (201 Created):**
```json
{
  "data": {
    "id": "...",
    "name": "Goal celebration",
    "type": "image",
    "originalName": "photo.jpg",
    "fileSize": 2048576,
    "fileUrl": "http://localhost:8080/uploads/4/f/3/uuid.jpg",
    "tags": [
      { "id": "...", "name": "Mbappé", "createdAt": "..." }
    ],
    "createdAt": "2026-05-21T12:00:00Z"
  }
}
```

| Status | Meaning |
|--------|---------|
| 201 | Media created |
| 400 | Missing name/file, invalid tag UUID |
| 413 | File exceeds 100MB limit |
| 415 | File type not image or video |

---

### List Media

**`GET /api/v1/media?limit=50&cursor=<opaque>&tag_id=<uuid>`**

Returns a cursor-paginated list of media items, ordered by creation date (newest first).

| Param | Type | Default | Max | Description |
|-------|------|---------|-----|-------------|
| `limit` | int | 50 | 100 | Items per page |
| `cursor` | string | — | — | Opaque cursor from previous response (omit for first page) |
| `tag_id` | uuid (repeatable) | — | 50 | Filter by tag UUID. Repeat for multi-tag AND (`?tag_id=A&tag_id=B`). Mutually exclusive with `tag_name`. |
| `tag_name` | string (repeatable) | — | 50 | Filter by tag name (exact match after server-side sanitization). Repeat for multi-tag AND (`?tag_name=Mbappé&tag_name=Ligue+1`). Mutually exclusive with `tag_id`. |

**Response (200 OK):**
```json
{
  "data": {
    "media": [
      {
        "id": "...",
        "name": "Goal celebration",
        "type": "image",
        "originalName": "photo.jpg",
        "fileSize": 2048576,
        "fileUrl": "http://localhost:8080/uploads/4/f/3/uuid.jpg",
        "tags": [ { "id": "...", "name": "Mbappé" } ],
        "createdAt": "2026-05-21T12:00:00Z"
      }
    ],
    "pagination": {
      "limit": 50,
      "nextCursor": "eyJ0IjoiMjAyNi0wNS0yMVQxMjowMDowMFoiLCJpIjoiLi4uIn0",
      "hasMore": true
    }
  }
}
```

---

### Get a Media

**`GET /api/v1/media/{id}`**

```bash
curl http://localhost:8080/api/v1/media/772a9622-... \
  -H "X-API-Key: your-api-key"
```

| Status | Meaning |
|--------|---------|
| 200 | Success |
| 400 | Invalid UUID format |
| 404 | Media not found |

---

### Delete a Media

**`DELETE /api/v1/media/{id}`**

Removes the media record from the database and deletes the associated file from storage. Tag associations (`media_tags`) are removed automatically via `ON DELETE CASCADE`.

```bash
curl -X DELETE http://localhost:8080/api/v1/media/772a9622-... \
  -H "X-API-Key: your-api-key"
```

**Response:** `204 No Content` (empty body)

| Status | Meaning |
|--------|---------|
| 204 | Media deleted |
| 400 | Invalid UUID format |
| 404 | Media not found |

---

### Attach Tags to an Existing Media

**`POST /api/v1/media/{id}/tags`**

Adds more tag associations to an existing media without re-uploading the
file. Same dual shape as the create endpoint: pass UUIDs you already
know, names that should be auto-created, or both.

```bash
curl -X POST http://localhost:8080/api/v1/media/772a9622-.../tags \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{"tag_names": ["UEFA Champions League"], "tags": ["550e8400-..."]}'
```

Re-attaching an already-linked tag is a no-op (idempotent). The response
returns the refreshed media so you can see the new tag list without a
follow-up `GET`.

| Status | Meaning |
|--------|---------|
| 200 | Tags attached; body is the refreshed media |
| 400 | Invalid UUID, malformed JSON, unsafe tag name, too many tags, or referenced tag id doesn't exist |
| 404 | Media not found |
| 415 | Content-Type is not `application/json` |

---

### Detach One Tag from a Media

**`DELETE /api/v1/media/{id}/tags/{tag_id}`**

Removes a single `(media_id, tag_id)` association. **The tag itself stays** —
other media that reference it are unaffected. Use this when you want to
correct over-tagging without nuking the tag for everyone.

```bash
curl -X DELETE http://localhost:8080/api/v1/media/772a9622-.../tags/550e8400-... \
  -H "X-API-Key: your-api-key"
```

Idempotent: removing a link that doesn't exist (e.g. someone unlinked it
already) also returns 204.

| Status | Meaning |
|--------|---------|
| 204 | Tag unlinked (or wasn't linked) |
| 400 | Invalid media or tag UUID |
| 404 | Media not found |

---

### Health Checks

| Endpoint | Type | Checks | Auth |
|----------|------|--------|------|
| `GET /healthz` | Liveness | Process alive | ❌ |
| `GET /readyz` | Readiness | DB reachable (2s timeout) | ❌ |

```json
// Healthy
{ "status": "ok" }

// Unhealthy (503)
{ "status": "error", "details": "database unreachable" }
```

---

### Prometheus Metrics

**`GET /metrics`** — Standard Prometheus exposition format, no auth required.

The exposed metrics map onto the four [Golden Signals](https://sre.google/sre-book/monitoring-distributed-systems/):

**Traffic & latency (HTTP)**
| Metric | Type | Labels | Usage |
|--------|------|--------|-------|
| `http_requests_total` | Counter | `method`, `route`, `status` | Traffic rate, error rate alerting |
| `http_request_duration_seconds` | Histogram | `method`, `route`, `status` | SLO monitoring (p50/p95/p99); the `status` label keeps fast errors from masking slow successes |
| `http_requests_in_flight` | Gauge | — | Autoscaling, capacity planning |

Buckets extend out to 60s so large uploads don't pile into `+Inf`.

**Errors (HTTP-level)**
| Metric | Type | Labels | Usage |
|--------|------|--------|-------|
| `http_panics_recovered_total` | Counter | — | Should be ~0 in steady state; spike → regression |
| `http_auth_failures_total` | Counter | `reason` (`missing`/`invalid`) | Brute-force detection |
| `http_rate_limit_rejections_total` | Counter | — | Capacity vs. limit-too-tight signal |

**Saturation**
| Metric | Type | Labels | Usage |
|--------|------|--------|-------|
| `media_api_*` DB pool family | Gauge/Counter | — | `*_in_use`, `*_wait_count`, `*_wait_duration_seconds`, etc. from `collectors.NewDBStatsCollector` — pool exhaustion detection |
| `job_queue_pending` | Gauge | — | Due-pending job count; sustained climb = worker pool falling behind |
| `job_queue_running` | Gauge | — | Currently-running jobs; useful with worker concurrency to spot a stuck pool |
| `job_queue_oldest_pending_age_seconds` | Gauge | — | Worst-case wait time for an enqueued job |
| `jobs_processed_total` | Counter | `type`, `outcome` (`completed`/`failed`/`unknown_type`) | Job throughput & error rate |
| `job_duration_seconds` | Histogram | `type` | Per-job-type latency (buckets up to 10 min) |

Standard Go runtime collectors (goroutines, GC, RSS) are also exposed automatically via `promhttp.Handler()`.

> Labels use chi route patterns (`/api/v1/media/{id}`) to avoid cardinality explosion from UUID paths.

**Example Prometheus scrape config:**
```yaml
scrape_configs:
  - job_name: media-api
    static_configs:
      - targets: ['localhost:8080']
```

---

### Idempotent Uploads

To prevent duplicate media creation on retries (network timeouts, mobile connectivity, load balancer retries), clients can send an `Idempotency-Key` header with `POST /api/v1/media`:

```bash
curl -X POST http://localhost:8080/api/v1/media \
  -H "X-API-Key: $API_KEY" \
  -H "Idempotency-Key: 550e8400-e29b-41d4-a716-446655440000" \
  -F "name=Goal celebration" \
  -F "file=@photo.jpg"
```

**Behavior:**
- **First call**: processed normally, response cached (24h TTL).
- **Retry (same key)**: cached response replayed, no duplicate created. Response includes `Idempotency-Replayed: true` header.
- **No header**: backward compatible, processed normally.
- **Error responses (4xx/5xx)**: NOT cached — client can fix the request and retry with the same key.

**Key format**: any non-empty string (UUID recommended).

---

### Input Validation

All tag and media names go through a Unicode-aware sanitization pipeline. The approach is **blocklist-based** (reject specific dangerous categories) rather than allowlist-based, so all scripts are supported out of the box.

**Allowed** (all scripts welcome):

| Category | Examples |
|----------|---------|
| Latin with accents | Mbappé, Müller, Çalhanoğlu |
| CJK ideographs | 田中将大, 손흥민, 大谷翔平 |
| Arabic, Hebrew, Devanagari | محمد صلاح, विराट कोहली |
| Emoji | ⚽ 🏆 🇫🇷 |
| Numbers, punctuation | Ligue 1, U-21, Player's Highlight |

**Rejected** (invisible or dangerous):

| Category | Codepoints | Why |
|----------|-----------|-----|
| Control characters | `U+0000–U+001F`, `U+007F–U+009F` | Break logs, JSON, terminals |
| Zero-width characters | `U+200B–U+200F`, `U+FEFF` | Invisible, cause search mismatches |
| Bidi overrides | `U+202A–U+202E`, `U+2066–U+2069` | Text renders differently than stored |
| Replacement char | `U+FFFD` | Indicates upstream encoding errors |
| Deprecated tag chars | `U+E0001–U+E007F` | No legitimate use |

**Normalized** (transformed, not rejected):

| Input | Output | Why |
|-------|--------|-----|
| NFD `e` + `́` | NFC `é` | Canonical form — prevents duplicate tags |
| Unicode whitespace (`\u00A0`, `\u3000`…) | ASCII space | Consistent search/display |
| Multiple spaces | Single space | Clean display |
| Tabs, newlines | Space | From copy-paste |

**Limits:**
- Max name length: 255 characters
- Max tags per media: 50

---

## Configuration

All settings are read from real environment variables — the app does **not**
auto-source `.env`. For local development the built-in defaults (see
`config/config.go`) already match `docker-compose.yml`, so `make run` works
out of the box without any config file.

`.env.example` documents every supported variable and doubles as a starting
template:

```bash
cp .env.example .env
# Either export the variables (export $(grep -v '^#' .env | xargs))
# or rely on the built-in dev defaults baked into config/config.go.
# docker-compose.yml also reads .env automatically for POSTGRES_* overrides.
```

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:8080` | Server bind address |
| `BASE_URL` | `http://localhost:8080` | Public base URL for file URLs |
| `LOG_FORMAT` | `json` | Log format: `json` (production) or `text` (development) |
| `DATABASE_URL` | — | PostgreSQL connection string |
| `DB_MAX_OPEN_CONNS` | `25` | Max open DB connections |
| `DB_MAX_IDLE_CONNS` | `5` | Max idle DB connections |
| `DB_CONN_MAX_LIFETIME` | `300` | Max connection lifetime (seconds) |
| `DB_CONN_MAX_IDLE_TIME` | `60` | Max idle time before close (seconds) |
| `STORAGE_BACKEND` | `local` | Storage: `local` or `s3` |
| `UPLOAD_DIR` | `./uploads` | Local file storage directory |
| `MAX_UPLOAD_SIZE` | `104857600` | Max upload size in bytes (100MB) |
| `API_KEY` | — | API key for authentication (empty = auth disabled) |
| `RATE_LIMIT_RPS` | `10` | Sustained requests per second per IP |
| `RATE_LIMIT_BURST` | `30` | Maximum burst size before throttling |
| `REQUEST_TIMEOUT` | `30` | Per-request context deadline (seconds) |
| `SHUTDOWN_TIMEOUT` | `30` | Graceful shutdown timeout (seconds) |

### S3 Storage Configuration

When `STORAGE_BACKEND=s3`:

| Variable | Description |
|----------|-------------|
| `S3_BUCKET` | Bucket name |
| `S3_REGION` | AWS region (e.g., `eu-west-1`) |
| `S3_ENDPOINT` | Custom endpoint for MinIO/DO Spaces |
| `S3_ACCESS_KEY` | Access key ID — dev only; leave empty in production and rely on the IAM role attached to the compute (triggers a startup warning when set) |
| `S3_SECRET_KEY` | Secret access key — same caveat as above |
| `S3_PREFIX` | Key prefix (e.g., `media/`) |
| `S3_CDN_URL` | CDN base URL for public file URLs |

**Credentials in production**: leave `S3_ACCESS_KEY` / `S3_SECRET_KEY` empty.
The AWS SDK's default credential chain picks up the IAM role on the compute
(IRSA on EKS, task role on ECS/Fargate, instance profile on EC2, …) and
delivers short-lived rotated STS credentials. Minimum IAM policy: `s3:PutObject`
+ `s3:DeleteObject` on `arn:aws:s3:::<bucket>/<prefix>/*`. No `s3:GetObject`
needed (the CDN serves reads).

**Private-tier media**: `port.FileStorage.URLWithExpiry(ctx, baseURL, path, ttl)`
issues short-lived presigned URLs (S3 SigV4) — bucket can be fully private.
The default `URL()` still returns plain URLs; presigning is plumbing waiting
on a private-tier product decision.

### Background Jobs

The app ships with a backend-agnostic background-job runner. The plumbing is
wired end-to-end (queue + worker pool + handler registry + metrics + graceful
shutdown); the only thing missing is real handler implementations, which are
intentionally left out of scope. A `noop` handler ships as a smoke test for
the full pipeline.

| Variable | Default | Description |
|----------|---------|-------------|
| `JOB_QUEUE_BACKEND` | `postgres` | `postgres` (in-process worker, SKIP LOCKED) or `sqs` (Enqueue-only; Lambda runs handlers) |
| `JOB_WORKER_ENABLED` | `true` | Start the in-process worker (postgres backend only). Set `false` on API replicas that only enqueue |
| `JOB_WORKER_CONCURRENCY` | `4` | Number of concurrent polling goroutines |
| `JOB_POLL_INTERVAL` | `2` | Per-goroutine poll cadence in seconds |
| `JOB_RETENTION_DAYS` | `7` | Days to retain completed/failed jobs before cleanup deletes them |
| `JOB_CLEANUP_INTERVAL` | `24` | Cleanup-loop cadence in hours |
| `JOB_STATS_INTERVAL` | `15` | Saturation gauge sampling cadence in seconds |
| `SQS_QUEUE_URL` | — | SQS queue URL (required when `JOB_QUEUE_BACKEND=sqs`) |
| `SQS_REGION` | — | AWS region for the SQS queue |

See [DESIGN.md → Background Jobs](DESIGN.md#background-jobs) for the lifecycle,
the plug-in pattern for new handlers, and the producer/consumer split.

---

## Docker

### Build the image

```bash
docker build -t media-api .
```

The multi-stage Dockerfile produces a **14MB distroless image** running as non-root (UID 65534).

### Run with Docker

```bash
docker run -p 8080:8080 \
  -e DATABASE_URL=postgres://... \
  -e API_KEY=your-secret-key \
  media-api
```

---

## CI/CD

### CI Pipeline (`.github/workflows/ci.yml`)

Runs on every push and PR to `main`:

| Job | Description |
|-----|-------------|
| **Lint** | `go vet` + `staticcheck` |
| **Unit Tests** | `-race -short`, coverage artifact |
| **E2E Tests** | testcontainers (PostgreSQL + MinIO) |
| **Build** | Compile binary, upload artifact |

### CD Pipeline (`.github/workflows/cd.yml`)

Runs on push to `main` and semver tags (`v*.*.*`):
- Builds and validates the Docker image (build-only, no push)

---

## Project Structure

```
cmd/api/main.go              — Entrypoint, DI wiring, graceful shutdown
config/                      — Environment-based configuration
internal/
  domain/                    — Pure business entities (zero dependencies)
  port/                      — Interfaces (driven + driving ports)
  service/                   — Business logic and orchestration
  adapter/
    http/handler/            — HTTP request handlers (tag, media, health)
    http/middleware/          — Logging, recovery, auth, CORS, rate limit,
                               security headers, request-id, metrics
    http/dto/                — Request/response data transfer objects
    postgres/                — Database repository implementations
    server/                  — Chi router composition
    storage/local/           — Hash-sharded local file storage
    storage/s3/              — S3-compatible cloud storage
  e2e/                       — End-to-end tests with testcontainers
```

See [DESIGN.md](DESIGN.md) for architecture decisions and trade-offs.
