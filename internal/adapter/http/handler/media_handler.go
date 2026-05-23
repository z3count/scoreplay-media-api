package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/adapter/http/dto"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
	"github.com/scoreplay/media-api/internal/service"
)

// MediaHandler handles HTTP requests for media-related endpoints.
//
// Endpoints:
//   - POST   /api/v1/media                        → Create (multipart file upload)
//   - GET    /api/v1/media                        → List (paginated, optional tag filter)
//   - GET    /api/v1/media/{id}                   → GetByID
//   - DELETE /api/v1/media/{id}                   → Delete (removes DB record + file)
//   - POST   /api/v1/media/{id}/tags              → AttachTags (link extra tags)
//   - DELETE /api/v1/media/{id}/tags/{tag_id}     → DetachTag (unlink one tag)
type MediaHandler struct {
	service       *service.MediaService
	logger        *slog.Logger
	maxUploadSize int64
}

// NewMediaHandler creates a MediaHandler.
//
// maxUploadSize is the maximum allowed upload size in bytes. This is enforced
// via http.MaxBytesReader to cut connections early for oversized uploads.
func NewMediaHandler(svc *service.MediaService, logger *slog.Logger, maxUploadSize int64) *MediaHandler {
	return &MediaHandler{service: svc, logger: logger, maxUploadSize: maxUploadSize}
}

// Create handles POST /api/v1/media.
//
// Request: multipart/form-data with fields:
//   - name (string, required): human-readable media name
//   - tags (repeated string): tag UUIDs to associate (pre-known tags)
//   - tag_names (repeated string): tag names; the service auto-creates any
//     that don't exist (idempotent CreateOrGet). Editors typically use this
//     instead of looking up UUIDs first.
//   - file (binary, required): the media file
//
// `tags` and `tag_names` are additive — a request can use either, both, or
// neither; the service merges and dedupes. The combined set caps at 50.
//
// Flow:
//   1. Apply MaxBytesReader to limit request body size (DoS protection).
//   2. Parse multipart form (10MB memory buffer, rest goes to temp files).
//   3. Extract and validate the "name" field.
//   4. Extract and validate "tags" field (parse UUIDs, skip invalid ones).
//   5. Extract the uploaded file.
//   6. Content-sniff the file (read first 512 bytes, seek back).
//   7. Delegate to MediaService.Create.
//   8. Build and return the response.
//
// Error responses:
//   - 400: missing name, missing file, invalid JSON/multipart
//   - 413: file exceeds max upload size
//   - 415: file type not image or video
//   - 422: referenced tag ID does not exist
//   - 500: storage or database error
func (h *MediaHandler) Create(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaWrite) {
		return
	}
	// Step 1: Limit body size.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadSize)

	// Step 2: Parse multipart. maxMemory is the cumulative budget for file
	// parts kept in RAM; anything above goes to an OS temp file that
	// MultipartForm.RemoveAll cleans up below. We keep it small (1 MiB) so
	// that the file part — the only large payload — always spills to disk
	// and we never hold the body in memory. Without this, N concurrent
	// uploads of ≤maxMemory bytes each can consume N × maxMemory of RAM and
	// OOM the process. Form values (name, tags) are small and unaffected.
	const multipartMaxMemoryBytes = 1 << 20 // 1 MiB
	if err := r.ParseMultipartForm(multipartMaxMemoryBytes); err != nil {
		if err.Error() == "http: request body too large" {
			writeError(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "file exceeds maximum upload size")
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_MULTIPART", "request must be multipart/form-data")
		return
	}
	defer r.MultipartForm.RemoveAll() //nolint:errcheck

	// Step 3: Extract name.
	name := r.FormValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "name field is required")
		return
	}

	// Step 4: Parse tag IDs and tag names. Both fields are repeatable and
	// can be mixed; the service merges them into a single deduped set.
	tagStrings := r.MultipartForm.Value["tags"]
	tagIDs := make([]uuid.UUID, 0, len(tagStrings))
	for _, s := range tagStrings {
		if s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tag ID: "+s)
			return
		}
		tagIDs = append(tagIDs, id)
	}
	tagNames := nonEmpty(r.MultipartForm.Value["tag_names"])

	// Step 5: Extract file.
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "file field is required")
		return
	}
	defer file.Close()

	// Step 6: Content sniffing.
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_FILE", "could not read file")
		return
	}
	contentType := http.DetectContentType(buf[:n])
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not process file")
		return
	}

	// Step 7: Delegate to service.
	input := service.CreateInput{
		Name:        name,
		File:        file,
		FileName:    header.Filename,
		FileSize:    header.Size,
		TagIDs:      tagIDs,
		TagNames:    tagNames,
		ContentType: contentType,
	}

	media, err := h.service.Create(r.Context(), input)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	resp := h.mediaToResponse(media)
	writeJSON(w, http.StatusCreated, map[string]interface{}{"data": resp})
}

// GetByID handles GET /api/v1/media/{id}.
//
// Path parameter:
//   - id: UUID of the media to retrieve.
//
// Returns the full media record including associated tags and a file URL.
//
// Error responses:
//   - 400: invalid UUID format in path
//   - 404: no media with the given ID
//   - 500: database error
func (h *MediaHandler) GetByID(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaRead) {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid media ID format")
		return
	}

	media, err := h.service.GetByID(r.Context(), id)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	resp := h.mediaToResponse(media)
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": resp})
}

// List handles GET /api/v1/media.
//
// Query parameters:
//   - limit (int, optional): max items per page. Default: 50, max: 100.
//   - cursor (string, optional): opaque cursor from a previous response.
//   - tag_id (uuid, repeatable, optional): filter by tag UUID(s).
//   - tag_name (string, repeatable, optional): filter by tag name(s).
//     Sanitized the same way as on tag creation.
//
// When tag_id or tag_name is repeated, results match media tagged with ALL
// listed tags (AND semantics). tag_id and tag_name are mutually exclusive:
// sending both returns 400.
//
// Returns a page of media ordered by creation date (newest first) and a
// nextCursor for the following page (empty when there are no more items).
func (h *MediaHandler) List(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaRead) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")

	filter, err := parseMediaFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}

	mediaList, nextCursor, err := h.service.List(r.Context(), limit, cursor, filter)
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	mediaResponses := make([]dto.MediaResponse, len(mediaList))
	for i, m := range mediaList {
		mediaResponses[i] = h.mediaToResponse(m)
	}

	resp := dto.ListMediaResponse{
		Media: mediaResponses,
		Pagination: dto.PaginationResponse{
			Limit:      len(mediaResponses),
			NextCursor: nextCursor,
			HasMore:    nextCursor != "",
		},
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": resp})
}

// Delete handles DELETE /api/v1/media/{id}.
//
// Removes the media record from the database and deletes the associated file
// from storage. The media_tags associations are deleted via ON DELETE CASCADE.
//
// Error responses:
//   - 400: invalid UUID format in path
//   - 404: no media with the given ID
//   - 500: database or storage error
func (h *MediaHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaWrite) {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid media ID format")
		return
	}

	if err := h.service.Delete(r.Context(), id); err != nil {
		h.handleError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AttachTags handles POST /api/v1/media/{id}/tags.
//
// Body: {"tags": ["<uuid>", ...], "tag_names": ["...", ...]}.
// Either or both fields may be present. Names are auto-created via
// CreateOrGet; the combined set is deduped server-side. Re-attaching a tag
// that's already linked is a no-op (idempotent).
//
// Status codes:
//   - 200 OK             — returns the refreshed media (full body)
//   - 400 VALIDATION     — invalid UUID, malformed JSON, unsafe tag name, too many tags
//   - 404 NOT_FOUND      — no media with that id, or a referenced tag id doesn't exist
//   - 415                — Content-Type is not application/json (CSRF guard)
func (h *MediaHandler) AttachTags(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaWrite) {
		return
	}
	idStr := chi.URLParam(r, "id")
	mediaID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid media ID format")
		return
	}

	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "INVALID_CONTENT_TYPE", "Content-Type must be application/json")
		return
	}

	var req dto.AttachTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
		return
	}

	tagIDs := make([]uuid.UUID, 0, len(req.Tags))
	for _, s := range req.Tags {
		if s == "" {
			continue
		}
		parsed, err := uuid.Parse(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tag ID: "+s)
			return
		}
		tagIDs = append(tagIDs, parsed)
	}

	media, err := h.service.AttachTags(r.Context(), mediaID, tagIDs, nonEmpty(req.TagNames))
	if err != nil {
		h.handleError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"data": h.mediaToResponse(media)})
}

// DetachTag handles DELETE /api/v1/media/{id}/tags/{tag_id}.
//
// Removes one association row from media_tags. The tag itself stays put —
// other media that reference it are unaffected.
//
// Status codes:
//   - 204 No Content     — unlinked (or wasn't linked; DELETE is idempotent)
//   - 400 VALIDATION     — invalid media or tag UUID
//   - 404 NOT_FOUND      — no media with that id
func (h *MediaHandler) DetachTag(w http.ResponseWriter, r *http.Request) {
	if !requireScope(w, r, ScopeMediaWrite) {
		return
	}
	mediaID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid media ID format")
		return
	}
	tagID, err := uuid.Parse(chi.URLParam(r, "tag_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid tag ID format")
		return
	}

	if err := h.service.DetachTag(r.Context(), mediaID, tagID); err != nil {
		h.handleError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleError maps domain errors to HTTP status codes.
func (h *MediaHandler) handleError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, domain.ErrUnsupportedMediaType):
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrFileTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", err.Error())
	default:
		h.logger.Error("unexpected error", "error", err, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "an unexpected error occurred")
	}
}

// parseMediaFilter reads repeated `tag_id` and `tag_name` query parameters
// and returns a populated MediaFilter. Returns an error if both kinds are
// present (mutually exclusive) or if any tag_id fails to parse as a UUID.
// Empty values in the repeated lists are silently skipped. TagNames are
// left raw — the service layer runs SanitizeName on each entry.
func parseMediaFilter(r *http.Request) (port.MediaFilter, error) {
	tagIDStrs := nonEmpty(r.URL.Query()["tag_id"])
	tagNames := nonEmpty(r.URL.Query()["tag_name"])

	if len(tagIDStrs) > 0 && len(tagNames) > 0 {
		return port.MediaFilter{}, errors.New("tag_id and tag_name are mutually exclusive")
	}

	var filter port.MediaFilter
	for _, s := range tagIDStrs {
		parsed, err := uuid.Parse(s)
		if err != nil {
			return port.MediaFilter{}, fmt.Errorf("invalid tag_id format: %s", s)
		}
		filter.TagIDs = append(filter.TagIDs, parsed)
	}
	filter.TagNames = tagNames
	return filter, nil
}

// nonEmpty returns a copy of ss with empty strings removed. URL query
// parsing yields empty entries for `?tag_id=` (no value) and similar; we
// don't want those to count toward the filter set.
func nonEmpty(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// mediaToResponse converts a domain.Media to a MediaResponse DTO.
func (h *MediaHandler) mediaToResponse(m *domain.Media) dto.MediaResponse {
	tags := make([]dto.TagResponse, len(m.Tags))
	for i, t := range m.Tags {
		tags[i] = dto.TagResponse{
			ID:        t.ID.String(),
			Name:      t.Name,
			CreatedAt: t.CreatedAt,
		}
	}
	return dto.MediaResponse{
		ID:           m.ID.String(),
		Name:         m.Name,
		Type:         string(m.Type),
		OriginalName: m.OriginalName,
		FileSize:     m.FileSize,
		FileURL:      h.service.FileURL(m.FilePath),
		Tags:         tags,
		CreatedAt:    m.CreatedAt,
	}
}
