// Package port defines the interfaces (ports) that the service layer depends on.
// These interfaces form the boundary between business logic and infrastructure.
//
// In hexagonal architecture terms:
//   - TagRepository and MediaRepository are "driven ports" (outbound): the
//     service layer calls them to persist/retrieve data.
//   - The HTTP handlers are "driving ports" (inbound): they call the service layer.
//
// By depending on interfaces rather than concrete implementations, the service
// layer can be tested with mocks and the infrastructure can be swapped without
// changing business logic (e.g., switching from PostgreSQL to another DB).
package port

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
)

// TagCursor is the keyset position used to resume a paginated tag listing.
// Tags are ordered by (name ASC, id ASC); id is a tie-breaker even though
// name is currently UNIQUE — it guarantees deterministic ordering if that
// constraint ever changes.
//
// Cursors are opaque to API clients: the service layer encodes them as a
// base64 string before exposing them over HTTP.
type TagCursor struct {
	Name string
	ID   uuid.UUID
}

// MediaCursor is the keyset position used to resume a paginated media listing.
// Media is ordered by (created_at DESC, id DESC); id is required to break ties
// when multiple rows share a timestamp (a real possibility on bulk imports or
// fast successive uploads).
type MediaCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// MediaFilter narrows a media listing to a subset of rows. The zero value
// applies no filter and lists all media.
//
// TagIDs and TagNames are mutually exclusive — the service/handler layer
// enforces this so the repository can assume at most one slice is non-empty.
// When a slice has N elements, the listing returns media tagged with ALL N
// tags (intersection semantics). Slices should be deduplicated by the
// caller so the HAVING COUNT predicate matches.
//
// We keep ID and name as separate slices so the wire-level distinction
// (UUID vs name) is preserved through the call stack and so the struct can
// grow new fields (q, date range, etc.) without breaking callers.
type MediaFilter struct {
	TagIDs   []uuid.UUID
	TagNames []string
}

// TagRepository defines the contract for tag persistence operations.
//
// Implementations must be safe for concurrent use (the HTTP server may call
// these methods from multiple goroutines simultaneously).
type TagRepository interface {
	// CreateOrGet inserts a new tag with the given name, or returns the existing
	// tag if a tag with that name already exists (idempotent upsert).
	//
	// This design avoids 409 Conflict errors in high-throughput scenarios where
	// thousands of media items may reference the same tags. The caller does not
	// need to check for existence before calling this method.
	//
	// The returned bool is true if a new tag was created, false if an existing
	// tag was returned. This allows the handler to return 201 vs 200 if desired.
	//
	// The name is expected to be pre-validated (trimmed, non-empty, ≤255 chars).
	CreateOrGet(ctx context.Context, name string) (domain.Tag, bool, error)

	// List returns up to limit tags ordered by (name ASC, id ASC).
	//
	// When cursor is nil, the first page is returned. When non-nil, only tags
	// strictly after the cursor position are returned. The returned next cursor
	// is non-nil iff there are more tags available beyond the returned page.
	//
	// Implementations should fetch one row beyond limit to compute hasMore
	// without paying for a separate COUNT(*) — the entire point of switching
	// away from LIMIT/OFFSET pagination.
	List(ctx context.Context, limit int, cursor *TagCursor) ([]domain.Tag, *TagCursor, error)

	// Rename changes the name of an existing tag and returns the updated row.
	//
	// The name is expected to be pre-validated by the service (sanitized,
	// non-empty, ≤255 chars). Errors:
	//   - domain.ErrNotFound: no tag with the given ID exists.
	//   - domain.ErrConflict: another tag already uses that name (UNIQUE
	//     constraint on tags.name).
	//
	// Renaming a tag to its current name is a no-op that returns the
	// existing row, not a conflict — the UNIQUE check excludes self.
	Rename(ctx context.Context, id uuid.UUID, name string) (domain.Tag, error)

	// Delete removes a tag by ID.
	//
	// Associations in media_tags are removed automatically via ON DELETE
	// CASCADE, so callers don't need to clean up media references.
	// Returns domain.ErrNotFound if no tag with the given ID exists.
	Delete(ctx context.Context, id uuid.UUID) error
}

// MediaRepository defines the contract for media persistence operations.
//
// The Create method expects to run within a database transaction to ensure
// atomicity when inserting the media record and its tag associations.
type MediaRepository interface {
	// Create inserts a new media record and its tag associations in a single
	// database transaction.
	//
	// The method must:
	//   1. Insert the media row (name, type, file_path, original_name, file_size)
	//   2. Insert rows into media_tags for each tag ID
	//   3. Return the complete Media with populated Tags (joined from the tags table)
	//
	// If any tag ID does not exist, the method must return domain.ErrNotFound
	// (the service layer will clean up the stored file in response).
	//
	// If the transaction fails at any point, it must be rolled back automatically.
	// The caller (service layer) is responsible for cleaning up the file on disk.
	Create(ctx context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error)

	// GetByID retrieves a single media item by its UUID, including its associated tags.
	//
	// Returns domain.ErrNotFound if no media with the given ID exists.
	// The returned Media.Tags slice will contain full Tag objects (id, name, createdAt),
	// not just tag IDs — this avoids N+1 queries in the handler.
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Media, error)

	// List returns up to limit media items ordered by (created_at DESC, id DESC).
	//
	// When cursor is nil, the first page is returned. When non-nil, only media
	// strictly older than the cursor position (per the ORDER BY) are returned.
	// The filter narrows the result; the zero value lists all media.
	// The returned next cursor is non-nil iff there are more media beyond the page.
	List(ctx context.Context, limit int, cursor *MediaCursor, filter MediaFilter) ([]*domain.Media, *MediaCursor, error)

	// Delete removes a media record and its tag associations from the database.
	//
	// Returns the file path of the deleted media so the caller can clean up
	// the associated file from storage. Returns domain.ErrNotFound if the
	// media ID does not exist.
	//
	// The media_tags rows are deleted automatically via ON DELETE CASCADE.
	Delete(ctx context.Context, id uuid.UUID) (filePath string, err error)

	// AttachTags adds tag associations to an existing media item.
	//
	// Already-existing (media_id, tag_id) pairs are silently kept — the
	// operation is idempotent so retrying is safe. The caller is
	// responsible for deduplicating and capping the tagIDs slice.
	//
	// Errors:
	//   - domain.ErrNotFound: no media with the given ID exists.
	//   - domain.ErrValidation: one or more tagIDs reference a tag that
	//     doesn't exist (foreign-key violation on insert). The whole
	//     statement is atomic — no partial attach.
	AttachTags(ctx context.Context, mediaID uuid.UUID, tagIDs []uuid.UUID) error

	// DetachTag removes a single (media_id, tag_id) association.
	//
	// The tag itself is not affected. Idempotent: removing a link that
	// doesn't exist is not an error.
	//
	// Returns domain.ErrNotFound if no media with the given ID exists.
	DetachTag(ctx context.Context, mediaID, tagID uuid.UUID) error
}

// IdempotencyStore stores and retrieves cached responses for idempotent
// request handling. When a client sends an Idempotency-Key header, the
// server stores the response on first execution and replays it on retries.
//
// This prevents duplicate resource creation when clients retry requests
// (network timeouts, mobile connectivity changes, load balancer retries).
type IdempotencyStore interface {
	// Get retrieves a cached response for the given key.
	// Returns the status code, response body, and true if found.
	// Returns 0, nil, false if the key does not exist or has expired.
	Get(ctx context.Context, key string) (statusCode int, body []byte, found bool, err error)

	// Set stores a response for the given key.
	// The key expires after the implementation-defined TTL (e.g. 24 hours).
	Set(ctx context.Context, key string, statusCode int, body []byte) error

	// Cleanup removes expired keys. Should be called periodically.
	Cleanup(ctx context.Context) error
}
