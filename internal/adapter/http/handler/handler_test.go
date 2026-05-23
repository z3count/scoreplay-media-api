package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
	"github.com/scoreplay/media-api/internal/service"
)

// --- Mock implementations ---

type mockStorage struct {
	saveFn func(ctx context.Context, reader io.Reader, ext string) (string, error)
}

func (m *mockStorage) Save(ctx context.Context, reader io.Reader, ext string) (string, error) {
	if m.saveFn != nil {
		return m.saveFn(ctx, reader, ext)
	}
	return "path", nil
}

func (m *mockStorage) Delete(_ context.Context, _ string) error { return nil }

func (m *mockStorage) URL(baseURL, path string) string {
	return baseURL + "/uploads/" + path
}

func (m *mockStorage) URLWithExpiry(_ context.Context, baseURL, path string, _ time.Duration) (string, error) {
	return m.URL(baseURL, path), nil
}

// Compile-time interface checks.
var _ port.FileStorage = (*mockStorage)(nil)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// withTestIdentity attaches an Identity to the request's context so the
// scope-check helper in handler/scope.go sees a valid principal. Unit
// tests bypass the auth middleware (which would normally populate the
// context); this gives them the same effect inline.
func withTestIdentity(r *http.Request) *http.Request {
	return r.WithContext(port.WithIdentity(r.Context(), domain.Identity{
		TenantID: domain.LegacyTenantID,
		Scopes:   []string{"admin:*"},
	}))
}

// --- Tag Handler Tests ---

// TestTagHandler_Create_Success tests the happy path for tag creation via HTTP.
func TestTagHandler_Create_Success(t *testing.T) {
	tagSvc := service.NewTagService(&testTagRepo{})
	h := NewTagHandler(tagSvc, testLogger())

	body := `{"name": "Test Tag"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatal("expected data in response")
	}
	if data["name"] != "Test Tag" {
		t.Errorf("expected name 'Test Tag', got %v", data["name"])
	}
}

// TestTagHandler_Create_InvalidJSON tests that malformed JSON returns 400.
func TestTagHandler_Create_InvalidJSON(t *testing.T) {
	tagSvc := service.NewTagService(&testTagRepo{})
	h := NewTagHandler(tagSvc, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/tags", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// TestTagHandler_Create_EmptyName tests that an empty tag name returns 400.
func TestTagHandler_Create_EmptyName(t *testing.T) {
	tagSvc := service.NewTagService(&testTagRepo{})
	h := NewTagHandler(tagSvc, testLogger())

	body := `{"name": "   "}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// TestTagHandler_Create_Security_WrongContentType tests that a request with
// Content-Type other than application/json is rejected with 415.
func TestTagHandler_Create_Security_WrongContentType(t *testing.T) {
	tagSvc := service.NewTagService(&testTagRepo{})
	h := NewTagHandler(tagSvc, testLogger())

	body := `{"name": "Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tags", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("Security: expected 415 for text/plain, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// TestTagHandler_List_Success tests the happy path for tag listing.
func TestTagHandler_List_Success(t *testing.T) {
	tagSvc := service.NewTagService(&testTagRepo{
		tags: []domain.Tag{
			{ID: uuid.New(), Name: "Tag1"},
			{ID: uuid.New(), Name: "Tag2"},
		},
	})
	h := NewTagHandler(tagSvc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tags?limit=10&offset=0", nil)
	rr := httptest.NewRecorder()

	h.List(rr, withTestIdentity(req))

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// TestMediaHandler_GetByID_InvalidUUID tests that a non-UUID path returns 400.
func TestMediaHandler_GetByID_InvalidUUID(t *testing.T) {
	mediaSvc := service.NewMediaService(&testMediaRepo{}, &testTagRepo{}, &mockStorage{}, "http://localhost", testLogger())
	h := NewMediaHandler(mediaSvc, testLogger(), 100*1024*1024)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/media/not-a-uuid", nil)
	rr := httptest.NewRecorder()

	// Set up chi URL params via route context.
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "not-a-uuid")
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	h.GetByID(rr, withTestIdentity(req))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

// TestMediaHandler_GetByID_NotFound tests that a valid UUID for a non-existent
// media returns 404.
func TestMediaHandler_GetByID_NotFound(t *testing.T) {
	mediaSvc := service.NewMediaService(&testMediaRepo{}, &testTagRepo{}, &mockStorage{}, "http://localhost", testLogger())
	h := NewMediaHandler(mediaSvc, testLogger(), 100*1024*1024)

	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/media/"+id.String(), nil)
	rr := httptest.NewRecorder()

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	req = req.WithContext(ctx)

	h.GetByID(rr, withTestIdentity(req))

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

// TestMediaHandler_Create_MissingName tests that a multipart upload without
// the "name" field returns 400.
func TestMediaHandler_Create_MissingName(t *testing.T) {
	mediaSvc := service.NewMediaService(&testMediaRepo{}, &testTagRepo{}, &mockStorage{}, "http://localhost", testLogger())
	h := NewMediaHandler(mediaSvc, testLogger(), 100*1024*1024)

	// Build multipart form without "name" field.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, _ := writer.CreateFormFile("file", "photo.jpg")
	part.Write([]byte("fake image data padded to be long enough for content detection"))
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/media", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// TestMediaHandler_Create_MissingFile tests that a multipart upload without
// the "file" field returns 400.
func TestMediaHandler_Create_MissingFile(t *testing.T) {
	mediaSvc := service.NewMediaService(&testMediaRepo{}, &testTagRepo{}, &mockStorage{}, "http://localhost", testLogger())
	h := NewMediaHandler(mediaSvc, testLogger(), 100*1024*1024)

	// Build multipart form with name but no file.
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("name", "Test Media")
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/media", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	h.Create(rr, withTestIdentity(req))

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// --- Test doubles that implement port interfaces ---

// testTagRepo is a simple in-memory tag repository for handler tests.
type testTagRepo struct {
	tags []domain.Tag
}

func (r *testTagRepo) CreateOrGet(_ context.Context, name string) (domain.Tag, bool, error) {
	tag := domain.Tag{ID: uuid.New(), Name: name}
	return tag, true, nil
}

func (r *testTagRepo) List(_ context.Context, limit int, cursor *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
	if r.tags == nil {
		return []domain.Tag{}, nil, nil
	}
	return r.tags, nil, nil
}

func (r *testTagRepo) Rename(_ context.Context, id uuid.UUID, name string) (domain.Tag, error) {
	return domain.Tag{ID: id, Name: name}, nil
}

func (r *testTagRepo) Delete(_ context.Context, _ uuid.UUID) error {
	return nil
}

// testMediaRepo is a simple in-memory media repository for handler tests.
type testMediaRepo struct{}

func (r *testMediaRepo) Create(_ context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
	media.ID = uuid.New()
	media.Tags = []domain.Tag{}
	return media, nil
}

func (r *testMediaRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Media, error) {
	return nil, domain.ErrNotFound
}

func (r *testMediaRepo) List(_ context.Context, limit int, cursor *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
	return []*domain.Media{}, nil, nil
}

func (r *testMediaRepo) Delete(_ context.Context, id uuid.UUID) (string, error) {
	return "", domain.ErrNotFound
}

func (r *testMediaRepo) AttachTags(_ context.Context, _ uuid.UUID, _ []uuid.UUID) error {
	return nil
}

func (r *testMediaRepo) DetachTag(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
	return nil
}
