package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// allowedImageTypes maps MIME types detected by http.DetectContentType to
// their canonical file extensions. We only allow real image/video formats.
//
// Why content sniffing instead of trusting Content-Type header?
// A malicious client can set any Content-Type header. By reading the first
// 512 bytes of the file and using Go's signature-based detection, we verify
// the actual file content matches an allowed type.
var allowedMIMETypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",

	"video/mp4":  ".mp4",
	"video/webm": ".webm",
	// Note: http.DetectContentType may return "application/octet-stream" for
	// some video formats. We handle this by also checking the original extension
	// for known video formats. See detectMediaType for details.
}

// knownVideoExtensions is a fallback set for video files whose binary signature
// is not recognized by http.DetectContentType (which returns "application/octet-stream").
// In such cases, we check the original filename extension as a secondary signal.
var knownVideoExtensions = map[string]bool{
	".mp4":  true,
	".webm": true,
	".mov":  true,
	".avi":  true,
	".mkv":  true,
}

// MediaService handles business logic for media operations.
//
// It orchestrates the multi-step process of creating media:
//   1. Validate inputs (name, tags, file content type)
//   2. Resolve tag names via the tag repository (auto-create if missing)
//   3. Store the file via FileStorage
//   4. Insert the DB record via MediaRepository (in a transaction)
//   5. If step 4 fails, delete the file from step 3 (compensating action)
//
// This manual rollback pattern maintains file/DB consistency without requiring
// distributed transactions or an outbox pattern (which would be overkill here).
type MediaService struct {
	repo    port.MediaRepository
	tagRepo port.TagRepository
	storage port.FileStorage
	baseURL string
	logger  *slog.Logger
}

// NewMediaService creates a MediaService with injected dependencies.
//
// Parameters:
//   - repo: database repository for media persistence.
//   - tagRepo: tag repository, used to resolve tag names to UUIDs (and
//     auto-create tags that don't exist yet) during media upload.
//   - storage: file storage backend (local disk or S3).
//   - baseURL: public base URL for constructing file URLs in responses.
//   - logger: structured logger for recording operations and errors.
func NewMediaService(repo port.MediaRepository, tagRepo port.TagRepository, storage port.FileStorage, baseURL string, logger *slog.Logger) *MediaService {
	return &MediaService{
		repo:    repo,
		tagRepo: tagRepo,
		storage: storage,
		baseURL: baseURL,
		logger:  logger,
	}
}

// CreateInput encapsulates all inputs needed to create a media item.
// This struct decouples the service from HTTP-specific types (multipart.File,
// *http.Request, etc.), making the service testable with plain io.Reader.
//
// Tags can be supplied two ways and both are merged into the final set:
//   - TagIDs: pre-known tag UUIDs (caller looked them up via List/Create).
//   - TagNames: human-readable names; the service sanitizes and auto-creates
//     each name if it doesn't exist (idempotent CreateOrGet semantics). This
//     makes the common editorial flow ("tag this with Mbappé + Ligue 1") a
//     single request instead of three.
type CreateInput struct {
	Name        string      // Human-readable name for the media
	File        io.Reader   // File content reader (from multipart upload)
	FileName    string      // Original client filename (for display, not storage)
	FileSize    int64       // File size in bytes (from Content-Length or header)
	TagIDs      []uuid.UUID // Pre-resolved tag UUIDs
	TagNames    []string    // Tag names; resolved (and auto-created) by the service
	ContentType string      // Detected content type (from sniffing, not header)
}

// Create orchestrates the full media creation flow: validate, store file, persist
// to database, and handle rollback on failure.
//
// Flow:
//   1. Validate the media name (non-empty, trimmed, ≤255 chars).
//   2. Enforce the combined tag count cap (TagIDs + TagNames ≤ 50).
//   3. Resolve TagNames via CreateOrGet, auto-creating any that don't exist.
//   4. Deduplicate the combined tag ID set.
//   5. Determine media type and file extension from content type.
//   6. Save the file to storage (streaming, no full buffering).
//   7. Insert the media + tag associations into the database (transaction).
//   8. If step 7 fails → delete the file from step 6 (compensating action).
//
// Error handling:
//   - Validation errors → domain.ErrValidation (caller maps to HTTP 400)
//   - Unsupported file type → domain.ErrUnsupportedMediaType (HTTP 415)
//   - Tag not found → domain.ErrNotFound (HTTP 422)
//   - Storage/DB errors → wrapped error (HTTP 500)
//
// Corner cases:
//   - File content says "image/jpeg" but extension is ".exe" → we trust the
//     sniffed content type and use ".jpg" as extension. Client filename is
//     stored in DB for display only.
//   - Database insert fails after file is written → file is deleted (logged
//     if cleanup also fails, since the DB error takes precedence).
//   - Empty tag IDs → media is created with no tags (valid use case).
func (s *MediaService) Create(ctx context.Context, input CreateInput) (*domain.Media, error) {
	// Step 1: Validate and sanitize name (NFC, whitespace, unsafe chars).
	name, err := SanitizeName(input.Name)
	if err != nil {
		return nil, err
	}

	// Step 2: Validate combined tag count. The cap covers IDs + names so a
	// client can't bypass it by splitting tags across the two fields.
	if len(input.TagIDs)+len(input.TagNames) > maxTagsPerMedia {
		return nil, fmt.Errorf("%w: too many tags (max %d)", domain.ErrValidation, maxTagsPerMedia)
	}

	// Step 3: Resolve tag names to UUIDs, auto-creating any that don't exist.
	// We do this *before* writing the file so a malformed tag name fails the
	// upload without leaving an orphan file on disk. Tags that get created
	// as a side effect of a later failure remain — that's harmless: tags are
	// the cheap entity, idempotent on retry.
	resolvedFromNames := make([]uuid.UUID, 0, len(input.TagNames))
	for _, n := range input.TagNames {
		sanitized, err := SanitizeName(n)
		if err != nil {
			return nil, err
		}
		tag, _, err := s.tagRepo.CreateOrGet(ctx, sanitized)
		if err != nil {
			return nil, fmt.Errorf("resolve tag %q: %w", sanitized, err)
		}
		resolvedFromNames = append(resolvedFromNames, tag.ID)
	}

	// Step 4: Dedupe the combined ID set. A client might pass the same tag
	// via both `tags` (UUID) and `tag_names` (name); after CreateOrGet they
	// resolve to the same UUID and dedupe collapses them.
	tagIDs := deduplicateUUIDs(append(input.TagIDs, resolvedFromNames...))

	// Step 5: Determine media type and extension from content type.
	mediaType, ext, err := classifyContentType(input.ContentType, input.FileName)
	if err != nil {
		return nil, err
	}

	// Step 6: Save file to storage.
	storedPath, err := s.storage.Save(ctx, input.File, ext)
	if err != nil {
		return nil, fmt.Errorf("save file to storage: %w", err)
	}

	// Step 7: Insert into database.
	media := &domain.Media{
		Name:         name,
		Type:         mediaType,
		FilePath:     storedPath,
		OriginalName: sanitizeFilename(input.FileName),
		FileSize:     input.FileSize,
	}

	result, err := s.repo.Create(ctx, media, tagIDs)
	if err != nil {
		// Step 8: Compensating action — delete the file we just stored.
		s.logger.Error("db insert failed, cleaning up stored file",
			"path", storedPath,
			"error", err,
		)
		if delErr := s.storage.Delete(ctx, storedPath); delErr != nil {
			s.logger.Error("failed to cleanup file after db error",
				"path", storedPath,
				"cleanup_error", delErr,
				"original_error", err,
			)
		}
		return nil, fmt.Errorf("create media record: %w", err)
	}

	return result, nil
}

// GetByID retrieves a media item by its UUID and constructs the public file URL.
//
// Returns domain.ErrNotFound if no media with the given ID exists, allowing
// the handler to return an appropriate 404 response.
//
// The file URL is constructed dynamically using the configured base URL and
// the stored file path. This means the URL adapts automatically if the base
// URL changes (e.g., moving from localhost to a production domain).
func (s *MediaService) GetByID(ctx context.Context, id uuid.UUID) (*domain.Media, error) {
	return s.repo.GetByID(ctx, id)
}

// maxFilterTags caps the number of tag filters in a single List request,
// matching maxTagsPerMedia. Prevents abuse via huge ANY(...) arrays.
const maxFilterTags = 50

// List retrieves a page of media using cursor-based (keyset) pagination,
// ordered by creation date (newest first).
//
// Pagination bounds:
//   - limit is clamped to [1, 100]. If 0 or negative, defaults to 50.
//   - cursor is the opaque string returned by a previous call, or empty for
//     the first page.
//   - filter narrows the result; the zero value lists all media.
//
// Multi-tag filters use AND semantics (media tagged with ALL given tags).
// TagNames are run through SanitizeName so a query for "  Mbappé  " matches
// the stored canonical "Mbappé". Duplicate IDs/names are deduplicated so
// the AND count predicate matches.
//
// Returns the media for this page and the next cursor (empty string when
// there are no more pages). A malformed cursor, an invalid tag name, or
// more than maxFilterTags entries in either slice returns a wrapped
// domain.ErrValidation.
func (s *MediaService) List(ctx context.Context, limit int, cursor string, filter port.MediaFilter) ([]*domain.Media, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	c, err := DecodeMediaCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	if len(filter.TagIDs) > maxFilterTags || len(filter.TagNames) > maxFilterTags {
		return nil, "", fmt.Errorf("%w: too many tag filters (max %d)", domain.ErrValidation, maxFilterTags)
	}

	// Dedupe IDs. AND semantics use HAVING COUNT(DISTINCT) = N, so a
	// duplicate would inflate N and never match.
	filter.TagIDs = deduplicateUUIDs(filter.TagIDs)

	// Sanitize each tag name then dedupe. Sanitization runs first so that
	// "  Mbappé  " and "Mbappé" collapse to the same canonical entry.
	if len(filter.TagNames) > 0 {
		seen := make(map[string]struct{}, len(filter.TagNames))
		normalized := make([]string, 0, len(filter.TagNames))
		for _, name := range filter.TagNames {
			n, err := SanitizeName(name)
			if err != nil {
				return nil, "", err
			}
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			normalized = append(normalized, n)
		}
		filter.TagNames = normalized
	}

	media, next, err := s.repo.List(ctx, limit, c, filter)
	if err != nil {
		return nil, "", err
	}
	return media, EncodeMediaCursor(next), nil
}

// Delete removes a media item: first from the database, then the associated file.
//
// Deletion order is DB-first because the database is the source of truth.
// If the file cleanup fails after a successful DB delete, we log the error
// but still return success — the media is logically deleted and the orphaned
// file can be cleaned up by a periodic garbage collection job.
//
// Returns domain.ErrNotFound if the media ID does not exist.
func (s *MediaService) Delete(ctx context.Context, id uuid.UUID) error {
	filePath, err := s.repo.Delete(ctx, id)
	if err != nil {
		return err
	}

	// Best-effort file cleanup. Log failure but don't return error
	// since the DB record (source of truth) is already deleted.
	if err := s.storage.Delete(ctx, filePath); err != nil {
		s.logger.Error("failed to delete file after DB deletion",
			"media_id", id,
			"file_path", filePath,
			"error", err,
		)
	}

	return nil
}

// FileURL constructs the public URL for a media's file.
// Separated from GetByID so the handler can build the URL with the correct base.
func (s *MediaService) FileURL(filePath string) string {
	return s.storage.URL(s.baseURL, filePath)
}

// classifyContentType maps a detected MIME type to a domain.MediaType and file extension.
//
// Two-phase detection:
//   1. Check the allowedMIMETypes map for a direct match.
//   2. If the MIME type is "application/octet-stream" (common for video files
//      whose signatures aren't in Go's sniff table), fall back to checking
//      the original filename extension against known video formats.
//
// Returns domain.ErrUnsupportedMediaType if the content doesn't match any
// allowed format. This prevents uploading executables, scripts, or other
// potentially dangerous file types.
func classifyContentType(contentType, fileName string) (domain.MediaType, string, error) {
	if ext, ok := allowedMIMETypes[contentType]; ok {
		if strings.HasPrefix(contentType, "image/") {
			return domain.MediaTypeImage, ext, nil
		}
		return domain.MediaTypeVideo, ext, nil
	}

	// Fallback for "application/octet-stream": check filename extension.
	// This handles video formats like .mov and .mkv whose binary signatures
	// aren't recognized by http.DetectContentType.
	if contentType == "application/octet-stream" {
		ext := strings.ToLower(filepath.Ext(fileName))
		if knownVideoExtensions[ext] {
			return domain.MediaTypeVideo, ext, nil
		}
	}

	return "", "", fmt.Errorf("%w: %s", domain.ErrUnsupportedMediaType, contentType)
}

// deduplicateUUIDs removes duplicate UUIDs from a slice while preserving order.
//
// This is needed because a client might send the same tag ID multiple times
// in a single request. Without deduplication, we'd get a primary key violation
// on the media_tags table (or insert duplicate rows if the constraint is missing).
func deduplicateUUIDs(ids []uuid.UUID) []uuid.UUID {
	if len(ids) == 0 {
		return ids
	}

	seen := make(map[uuid.UUID]struct{}, len(ids))
	result := make([]uuid.UUID, 0, len(ids))

	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			result = append(result, id)
		}
	}

	return result
}

// sanitizeFilename extracts just the base name from a client-provided filename,
// removing any path components. This prevents path traversal attacks where a
// client sends a filename like "../../etc/passwd".
//
// Handles both Unix (/) and Windows (\) path separators, since the client
// may be running on any platform. On Linux, filepath.Base does not split on
// backslashes, so we normalize backslashes to forward slashes first.
//
// The sanitized name is stored in the database for display purposes only — it
// is NEVER used as a filesystem path. All storage paths use UUID-based names.
func sanitizeFilename(name string) string {
	// Normalize Windows backslashes to forward slashes.
	name = strings.ReplaceAll(name, "\\", "/")
	// filepath.Base extracts the last element of the path.
	return filepath.Base(name)
}

// AttachTags links additional tags to an existing media item.
//
// Mirrors the Create-time semantics: TagNames are sanitized and resolved
// via CreateOrGet (auto-create if missing), then merged with explicit
// TagIDs and deduped. The combined input is capped at maxTagsPerMedia —
// the same cap as Create, applied to the *input batch* (not the
// post-attach total, which would race anyway).
//
// Re-attaching an already-linked tag is a no-op thanks to the repo's
// ON CONFLICT DO NOTHING; the operation is idempotent so clients can
// safely retry.
//
// Returns the refreshed media (with the complete current tag list) so the
// caller doesn't need a follow-up GET to see the new state.
func (s *MediaService) AttachTags(ctx context.Context, mediaID uuid.UUID, tagIDs []uuid.UUID, tagNames []string) (*domain.Media, error) {
	if len(tagIDs)+len(tagNames) > maxTagsPerMedia {
		return nil, fmt.Errorf("%w: too many tags (max %d)", domain.ErrValidation, maxTagsPerMedia)
	}

	resolvedFromNames := make([]uuid.UUID, 0, len(tagNames))
	for _, n := range tagNames {
		sanitized, err := SanitizeName(n)
		if err != nil {
			return nil, err
		}
		tag, _, err := s.tagRepo.CreateOrGet(ctx, sanitized)
		if err != nil {
			return nil, fmt.Errorf("resolve tag %q: %w", sanitized, err)
		}
		resolvedFromNames = append(resolvedFromNames, tag.ID)
	}

	combined := deduplicateUUIDs(append(tagIDs, resolvedFromNames...))

	if err := s.repo.AttachTags(ctx, mediaID, combined); err != nil {
		return nil, err
	}

	return s.repo.GetByID(ctx, mediaID)
}

// DetachTag removes one tag association from a media item.
//
// The tag itself is not deleted — it stays available for other media.
// Idempotent: detaching a tag that wasn't attached is not an error.
// Returns domain.ErrNotFound if the media id is unknown.
func (s *MediaService) DetachTag(ctx context.Context, mediaID, tagID uuid.UUID) error {
	return s.repo.DetachTag(ctx, mediaID, tagID)
}
