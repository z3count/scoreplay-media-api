// Package handler implements the HTTP handlers (driving adapters) for the API.
//
// Handlers are responsible for:
//   - Parsing and validating HTTP requests (query params, JSON bodies, multipart forms)
//   - Calling the appropriate service method
//   - Mapping domain errors to HTTP status codes
//   - Serializing responses as JSON
//
// Handlers should be thin: no business logic lives here. If you find yourself
// writing an if/else that checks a business rule, it belongs in the service layer.
package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/adapter/http/dto"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/service"
)

// TagHandler handles HTTP requests for tag-related endpoints.
//
// Endpoints:
//   - POST   /api/v1/tags        → Create (idempotent create-or-get)
//   - GET    /api/v1/tags        → List (paginated)
//   - PATCH  /api/v1/tags/{id}   → Update (rename)
//   - DELETE /api/v1/tags/{id}   → Delete (cascades to media_tags)
type TagHandler struct {
	service *service.TagService
	logger  *slog.Logger
}

// NewTagHandler creates a TagHandler with the given service and logger.
func NewTagHandler(svc *service.TagService, logger *slog.Logger) *TagHandler {
	return &TagHandler{service: svc, logger: logger}
}

// Create handles POST /api/v1/tags.
//
// Expected request body: {"name": "string"}
//
// Behavior:
//   - Parses the JSON body into a CreateTagRequest DTO.
//   - Delegates to TagService.Create for validation and persistence.
//   - If the tag is newly created, returns 201 Created.
//   - If a tag with the same name already exists, returns 200 OK with the
//     existing tag (idempotent — no error, no conflict).
//
// Error responses:
//   - 400 Bad Request: malformed JSON or validation failure (empty name, too long).
//   - 500 Internal Server Error: unexpected database or server error.
//
// Corner cases:
//   - Empty body → 400 (JSON decode fails).
//   - Body with extra unknown fields → silently ignored (Go's default JSON behavior).
//   - Content-Type not application/json → still works (Go's json.Decoder doesn't
//     check Content-Type), but clients should send the correct header.
func (h *TagHandler) Create(w http.ResponseWriter, r *http.Request) {
	// Validate Content-Type to prevent CSRF via text/plain requests.
	// Browsers don't send preflight CORS for text/plain, so without this check
	// a malicious page could POST to the API without triggering CORS.
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "INVALID_CONTENT_TYPE", "Content-Type must be application/json")
		return
	}

	var req dto.CreateTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
		return
	}

	tag, created, err := h.service.Create(r.Context(), req.Name)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	resp := tagToResponse(tag)

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}

	writeJSON(w, status, map[string]interface{}{"data": resp})
}

// List handles GET /api/v1/tags.
//
// Query parameters:
//   - limit (int, optional): max items per page. Default: 50, max: 100.
//   - cursor (string, optional): opaque cursor from a previous response.
//     Omit on the first call.
//
// Returns a page of tags ordered by name and a nextCursor for the following
// page (empty when there are no more tags).
//
// Corner cases:
//   - No query params → first page with default limit (50).
//   - limit=abc (non-integer) → uses default (50).
//   - limit=500 → capped to 100 by service layer.
//   - Malformed cursor → 400 VALIDATION_ERROR.
func (h *TagHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")

	tags, nextCursor, err := h.service.List(r.Context(), limit, cursor)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	tagResponses := make([]dto.TagResponse, len(tags))
	for i, t := range tags {
		tagResponses[i] = tagToResponse(t)
	}

	resp := dto.ListTagsResponse{
		Tags: tagResponses,
		Pagination: dto.PaginationResponse{
			Limit:      len(tagResponses),
			NextCursor: nextCursor,
			HasMore:    nextCursor != "",
		},
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": resp})
}

// handleError maps domain errors to HTTP status codes and writes a JSON error response.
//
// This centralizes error mapping in one place, ensuring consistent error responses
// across all tag endpoints. New domain error types can be added here as the
// application grows.
func (h *TagHandler) handleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	default:
		h.logger.Error("unexpected error", "error", err, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
	}
}

// Update handles PATCH /api/v1/tags/{id}.
//
// Expected body: {"name": "string"}. The new name goes through the same
// sanitization pipeline as creation, and the tags.name UNIQUE constraint
// catches duplicates.
//
// Status codes:
//   - 200 OK — renamed (or no-op if name unchanged).
//   - 400 — invalid UUID or invalid JSON or invalid name.
//   - 404 — no tag with that id.
//   - 409 — the new name is already used by a different tag.
//   - 415 — Content-Type is not application/json (CSRF guard).
func (h *TagHandler) Update(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tag ID format")
		return
	}

	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "INVALID_CONTENT_TYPE", "Content-Type must be application/json")
		return
	}

	var req dto.UpdateTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
		return
	}

	tag, err := h.service.Rename(r.Context(), id, req.Name)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": tagToResponse(tag)})
}

// Delete handles DELETE /api/v1/tags/{id}.
//
// Removes the tag and (via ON DELETE CASCADE on media_tags) all of its
// media associations — media items themselves remain untouched, they
// just lose this tag.
//
// Status codes:
//   - 204 No Content — deleted.
//   - 400 — invalid UUID.
//   - 404 — no tag with that id.
func (h *TagHandler) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tag ID format")
		return
	}

	if err := h.service.Delete(r.Context(), id); err != nil {
		h.handleError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// tagToResponse converts a domain.Tag to a TagResponse DTO.
func tagToResponse(t domain.Tag) dto.TagResponse {
	return dto.TagResponse{
		ID:        t.ID.String(),
		Name:      t.Name,
		CreatedAt: t.CreatedAt,
	}
}
