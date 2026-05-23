// Package service implements the business logic layer of the application.
//
// Services orchestrate the flow between port interfaces (repositories, storage)
// and domain entities. They contain validation logic, business rules, and
// coordinate multi-step operations (e.g., storing a file + inserting a DB record).
//
// Services depend only on interfaces (ports) and domain types — never on
// concrete adapter implementations. This makes them fully unit-testable with mocks.
package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// maxTagNameLength is the maximum allowed length for a tag name.
// This prevents abuse (e.g., a malicious client sending a 10MB tag name)
// and keeps the data manageable for display and search.
const maxTagNameLength = 255

// TagService handles business logic for tag operations.
//
// It sits between the HTTP handler (which parses the request) and the
// repository (which persists the data). The service is responsible for:
//   - Input validation (non-empty, trimmed, length check)
//   - Calling the repository
//   - Pagination bounds enforcement
//
// It does NOT handle HTTP concerns (status codes, content types) — that's
// the handler's job.
type TagService struct {
	repo port.TagRepository
}

// NewTagService creates a TagService with the given repository.
// The repository is injected as an interface, enabling mock-based unit tests.
func NewTagService(repo port.TagRepository) *TagService {
	return &TagService{repo: repo}
}

// Create validates the tag name and delegates to the repository for idempotent
// creation (create-or-get semantics).
//
// Validation rules (via SanitizeName):
//   - NFC Unicode normalization (prevents duplicate tags from different encodings).
//   - Unicode whitespace normalized to ASCII space, multiple spaces collapsed.
//   - Leading/trailing whitespace stripped.
//   - Control characters, zero-width chars, bidi overrides rejected.
//   - Name must not be empty after processing.
//   - Name must not exceed 255 characters.
//
// Returns:
//   - The created or existing tag.
//   - A bool indicating whether the tag was newly created (true) or already
//     existed (false). This allows the handler to return 201 vs 200.
//   - An error wrapping domain.ErrValidation if the name is invalid.
//
// Corner cases:
//   - Name "  Mbappé  " → trimmed to "Mbappé" before insert.
//   - Name "" or "   " → returns domain.ErrValidation.
//   - Name with exactly 255 chars → accepted.
//   - Name with 256 chars → rejected.
//   - NFD "café" and NFC "café" → stored identically (NFC).
func (s *TagService) Create(ctx context.Context, name string) (domain.Tag, bool, error) {
	name, err := SanitizeName(name)
	if err != nil {
		return domain.Tag{}, false, err
	}

	tag, created, err := s.repo.CreateOrGet(ctx, name)
	if err != nil {
		return domain.Tag{}, false, fmt.Errorf("create or get tag: %w", err)
	}

	return tag, created, nil
}

// List retrieves a page of tags using cursor-based (keyset) pagination.
//
// Pagination bounds:
//   - limit is clamped to [1, 100]. If 0 or negative, defaults to 50.
//   - cursor is the opaque string returned by a previous call, or empty for
//     the first page.
//
// Returns the tags for this page and the next cursor (empty string when there
// are no more pages). A malformed cursor returns a wrapped domain.ErrValidation.
func (s *TagService) List(ctx context.Context, limit int, cursor string) ([]domain.Tag, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}

	c, err := DecodeTagCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	tags, next, err := s.repo.List(ctx, limit, c)
	if err != nil {
		return nil, "", err
	}
	return tags, EncodeTagCursor(next), nil
}

// Rename renames an existing tag.
//
// The new name is sanitized with the same pipeline used at creation so a
// rename to "  Mbappé  " ends up stored as the canonical "Mbappé". If the
// sanitized name happens to equal the current name, the repository
// returns the existing row unchanged — a renaming-to-self is a no-op,
// not a conflict.
//
// Errors:
//   - domain.ErrValidation wrapped: empty / oversized / unsafe name.
//   - domain.ErrNotFound: no tag with that id.
//   - domain.ErrConflict: another tag already uses the new name.
func (s *TagService) Rename(ctx context.Context, id uuid.UUID, name string) (domain.Tag, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return domain.Tag{}, err
	}
	return s.repo.Rename(ctx, id, sanitized)
}

// Delete removes a tag by ID. Returns domain.ErrNotFound if the tag does
// not exist. Associated media_tags rows are removed automatically by the
// schema's ON DELETE CASCADE, so the affected media transparently lose
// the tag without any application-level fan-out.
func (s *TagService) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}
