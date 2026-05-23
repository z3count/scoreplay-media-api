// Package dto defines the Data Transfer Objects used to serialize and
// deserialize HTTP request/response bodies.
//
// DTOs are deliberately separated from domain entities to:
//   1. Decouple the API contract from the internal data model. The domain
//      can evolve independently of the API surface.
//   2. Control exactly which fields are exposed to clients (e.g., FilePath
//      is internal and never exposed; fileUrl is computed).
//   3. Apply JSON naming conventions (camelCase) without polluting domain structs.
package dto

import "time"

// --- Tag DTOs ---

// CreateTagRequest is the expected JSON body for POST /api/v1/tags.
//
// Validation:
//   - Name is required, must not be empty or whitespace-only.
//   - Name must not exceed 255 characters.
//   - Leading/trailing whitespace is trimmed by the service layer.
type CreateTagRequest struct {
	Name string `json:"name"`
}

// UpdateTagRequest is the JSON body for PATCH /api/v1/tags/{id}.
// Same validation rules and sanitization as creation; the existing
// tags.name UNIQUE constraint enforces no-duplicate semantics.
type UpdateTagRequest struct {
	Name string `json:"name"`
}

// TagResponse is the JSON representation of a tag in API responses.
// Used both in individual tag responses and as elements of tag arrays.
type TagResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListTagsResponse wraps a list of tags with pagination metadata.
// The pagination object helps clients navigate through large result sets.
type ListTagsResponse struct {
	Tags       []TagResponse      `json:"tags"`
	Pagination PaginationResponse `json:"pagination"`
}

// PaginationResponse provides metadata for cursor-paginated list endpoints.
//
// Clients should treat NextCursor as opaque and pass it back verbatim as the
// `cursor` query parameter to fetch the next page. When HasMore is false,
// NextCursor is omitted from the JSON response.
//
// Note: there is intentionally no `total` field. Cursor pagination is the
// alternative to offset pagination precisely so we can avoid the linear
// COUNT(*) scan that totals would require on every list call. Clients that
// need approximate sizing should use a dedicated, cached endpoint.
type PaginationResponse struct {
	Limit      int    `json:"limit"`
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore"`
}

// --- Media DTOs ---

// MediaResponse is the JSON representation of a media item in API responses.
//
// Notable design choices:
//   - FileUrl is a computed field (not stored in DB) built from the base URL
//     and the internal file path. This means the URL adapts automatically
//     when the application moves to a different domain.
//   - Tags contains full tag objects (not just IDs) to avoid N+1 lookups
//     on the client side.
//   - FilePath is deliberately omitted (internal implementation detail).
type MediaResponse struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Type         string        `json:"type"`
	OriginalName string        `json:"originalName"`
	FileSize     int64         `json:"fileSize"`
	FileURL      string        `json:"fileUrl"`
	Tags         []TagResponse `json:"tags"`
	CreatedAt    time.Time     `json:"createdAt"`
}

// ListMediaResponse wraps a list of media items with pagination metadata.
type ListMediaResponse struct {
	Media      []MediaResponse    `json:"media"`
	Pagination PaginationResponse `json:"pagination"`
}

// AttachTagsRequest is the JSON body for POST /api/v1/media/{id}/tags.
//
// Same dual shape as the upload endpoint: clients can pass tag UUIDs they
// already know about and/or tag names (auto-created if missing). The two
// fields are additive and merged server-side.
type AttachTagsRequest struct {
	Tags     []string `json:"tags"`
	TagNames []string `json:"tag_names"`
}
