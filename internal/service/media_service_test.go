package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"

	"log/slog"
)

// mockMediaRepo is a test double for port.MediaRepository.
type mockMediaRepo struct {
	createFn  func(ctx context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error)
	getByIDFn func(ctx context.Context, id uuid.UUID) (*domain.Media, error)
	listFn       func(ctx context.Context, limit int, cursor *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error)
	deleteFn     func(ctx context.Context, id uuid.UUID) (string, error)
	attachTagsFn func(ctx context.Context, mediaID uuid.UUID, tagIDs []uuid.UUID) error
	detachTagFn  func(ctx context.Context, mediaID, tagID uuid.UUID) error
}

func (m *mockMediaRepo) Create(ctx context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
	return m.createFn(ctx, media, tagIDs)
}

func (m *mockMediaRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Media, error) {
	return m.getByIDFn(ctx, id)
}

func (m *mockMediaRepo) List(ctx context.Context, limit int, cursor *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
	if m.listFn != nil {
		return m.listFn(ctx, limit, cursor, filter)
	}
	return []*domain.Media{}, nil, nil
}

func (m *mockMediaRepo) Delete(ctx context.Context, id uuid.UUID) (string, error) {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return "", nil
}

func (m *mockMediaRepo) AttachTags(ctx context.Context, mediaID uuid.UUID, tagIDs []uuid.UUID) error {
	if m.attachTagsFn != nil {
		return m.attachTagsFn(ctx, mediaID, tagIDs)
	}
	return nil
}

func (m *mockMediaRepo) DetachTag(ctx context.Context, mediaID, tagID uuid.UUID) error {
	if m.detachTagFn != nil {
		return m.detachTagFn(ctx, mediaID, tagID)
	}
	return nil
}

// mockStorage is a test double for port.FileStorage.
type mockStorage struct {
	saveFn          func(ctx context.Context, reader io.Reader, ext string) (string, error)
	deleteFn        func(ctx context.Context, path string) error
	urlFn           func(baseURL, path string) string
	urlWithExpiryFn func(ctx context.Context, baseURL, path string, ttl time.Duration) (string, error)
}

func (m *mockStorage) Save(ctx context.Context, reader io.Reader, ext string) (string, error) {
	return m.saveFn(ctx, reader, ext)
}

func (m *mockStorage) Delete(ctx context.Context, path string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, path)
	}
	return nil
}

func (m *mockStorage) URL(baseURL, path string) string {
	if m.urlFn != nil {
		return m.urlFn(baseURL, path)
	}
	return baseURL + "/uploads/" + path
}

func (m *mockStorage) URLWithExpiry(ctx context.Context, baseURL, path string, ttl time.Duration) (string, error) {
	if m.urlWithExpiryFn != nil {
		return m.urlWithExpiryFn(ctx, baseURL, path, ttl)
	}
	return m.URL(baseURL, path), nil
}

// Compile-time check that mocks implement the interfaces.
var _ port.MediaRepository = (*mockMediaRepo)(nil)
var _ port.FileStorage = (*mockStorage)(nil)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestMediaService_Create_Success verifies the happy path: valid inputs result
// in file storage + DB insert + correct response.
func TestMediaService_Create_Success(t *testing.T) {
	tagID := uuid.New()
	mediaID := uuid.New()

	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, ext string) (string, error) {
			if ext != ".jpg" {
				t.Errorf("expected .jpg extension, got %q", ext)
			}
			return "a/b/c/test.jpg", nil
		},
	}

	repo := &mockMediaRepo{
		createFn: func(_ context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
			media.ID = mediaID
			media.Tags = []domain.Tag{{ID: tagID, Name: "test-tag"}}
			return media, nil
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "http://localhost:8080", newTestLogger())

	input := CreateInput{
		Name:        "Test Media",
		File:        strings.NewReader("fake-jpeg-content"),
		FileName:    "photo.jpg",
		FileSize:    100,
		TagIDs:      []uuid.UUID{tagID},
		ContentType: "image/jpeg",
	}

	media, err := svc.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if media.ID != mediaID {
		t.Errorf("expected media ID %s, got %s", mediaID, media.ID)
	}
	if media.Name != "Test Media" {
		t.Errorf("expected name 'Test Media', got %q", media.Name)
	}
	if media.Type != domain.MediaTypeImage {
		t.Errorf("expected type 'image', got %q", media.Type)
	}
}

// TestMediaService_Create_TagNamesAutoCreated verifies that names in
// CreateInput.TagNames are sanitized and resolved via TagRepository.CreateOrGet,
// then merged with any explicit TagIDs into the final association set.
func TestMediaService_Create_TagNamesAutoCreated(t *testing.T) {
	existingID := uuid.New()
	createdID := uuid.New()

	var createOrGetCalls []string
	tagRepo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			createOrGetCalls = append(createOrGetCalls, name)
			switch name {
			case "Mbappé":
				return domain.Tag{ID: existingID, Name: name}, false, nil
			case "Ligue 1":
				return domain.Tag{ID: createdID, Name: name}, true, nil
			}
			return domain.Tag{}, false, fmt.Errorf("unexpected name %q", name)
		},
	}

	var capturedTagIDs []uuid.UUID
	mediaRepo := &mockMediaRepo{
		createFn: func(_ context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
			capturedTagIDs = tagIDs
			media.ID = uuid.New()
			media.Tags = []domain.Tag{}
			return media, nil
		},
	}
	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) {
			return "path", nil
		},
	}

	svc := NewMediaService(mediaRepo, tagRepo, storage, "http://localhost", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "Photo",
		File:        strings.NewReader("data"),
		FileName:    "p.jpg",
		ContentType: "image/jpeg",
		TagNames:    []string{"  Mbappé  ", "Ligue 1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(createOrGetCalls) != 2 || createOrGetCalls[0] != "Mbappé" || createOrGetCalls[1] != "Ligue 1" {
		t.Errorf("CreateOrGet called with %v, want [Mbappé, Ligue 1]", createOrGetCalls)
	}
	if len(capturedTagIDs) != 2 {
		t.Fatalf("expected 2 resolved tag IDs, got %d", len(capturedTagIDs))
	}
}

// TestMediaService_Create_TagNamesMergeWithIDs verifies that the same tag
// passed via both TagIDs and TagNames collapses to a single association.
func TestMediaService_Create_TagNamesMergeWithIDs(t *testing.T) {
	tagID := uuid.New()

	tagRepo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			return domain.Tag{ID: tagID, Name: name}, false, nil
		},
	}

	var capturedTagIDs []uuid.UUID
	mediaRepo := &mockMediaRepo{
		createFn: func(_ context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
			capturedTagIDs = tagIDs
			media.Tags = []domain.Tag{}
			return media, nil
		},
	}

	svc := NewMediaService(mediaRepo, tagRepo, &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) { return "p", nil },
	}, "", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "X",
		File:        strings.NewReader("d"),
		FileName:    "x.jpg",
		ContentType: "image/jpeg",
		TagIDs:      []uuid.UUID{tagID},
		TagNames:    []string{"Mbappé"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedTagIDs) != 1 {
		t.Errorf("expected 1 tag ID after dedupe, got %d", len(capturedTagIDs))
	}
}

// TestMediaService_Create_TagNamesUnsafeRejected verifies that a tag_name
// failing SanitizeName aborts the upload before storage.Save runs (no orphan
// file).
func TestMediaService_Create_TagNamesUnsafeRejected(t *testing.T) {
	storageCalled := false
	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) {
			storageCalled = true
			return "p", nil
		},
	}
	tagRepo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, _ string) (domain.Tag, bool, error) {
			t.Error("CreateOrGet should not be called when sanitize fails")
			return domain.Tag{}, false, nil
		},
	}

	svc := NewMediaService(&mockMediaRepo{}, tagRepo, storage, "", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "X",
		File:        strings.NewReader("d"),
		FileName:    "x.jpg",
		ContentType: "image/jpeg",
		TagNames:    []string{"bad\x00name"},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
	if storageCalled {
		t.Error("storage.Save should not run when tag_name validation fails")
	}
}

// TestMediaService_Create_TagCapCombined verifies that the 50-tag cap applies
// to the combined IDs+names set, not each list independently.
func TestMediaService_Create_TagCapCombined(t *testing.T) {
	svc := NewMediaService(&mockMediaRepo{}, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	ids := make([]uuid.UUID, 30)
	for i := range ids {
		ids[i] = uuid.New()
	}
	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("tag-%d", i)
	}

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "X",
		File:        strings.NewReader("d"),
		FileName:    "x.jpg",
		ContentType: "image/jpeg",
		TagIDs:      ids,
		TagNames:    names,
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for combined cap, got %v", err)
	}
}

// TestMediaService_Create_EmptyName verifies that an empty media name is rejected.
func TestMediaService_Create_EmptyName(t *testing.T) {
	svc := NewMediaService(nil, &mockTagRepo{}, nil, "", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name: "   ",
		File: strings.NewReader("data"),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got: %v", err)
	}
}

// TestMediaService_Create_UnsupportedType verifies that unsupported content types
// (e.g., application/pdf) are rejected.
func TestMediaService_Create_UnsupportedType(t *testing.T) {
	svc := NewMediaService(nil, &mockTagRepo{}, nil, "", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "test",
		File:        strings.NewReader("data"),
		FileName:    "doc.pdf",
		ContentType: "application/pdf",
	})
	if !errors.Is(err, domain.ErrUnsupportedMediaType) {
		t.Errorf("expected ErrUnsupportedMediaType, got: %v", err)
	}
}

// TestMediaService_Create_DBFailure_FileCleanup verifies that when the database
// insert fails after the file has been stored, the file is cleaned up
// (compensating action / manual rollback).
func TestMediaService_Create_DBFailure_FileCleanup(t *testing.T) {
	var deletedPath string

	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) {
			return "a/b/c/test.jpg", nil
		},
		deleteFn: func(_ context.Context, path string) error {
			deletedPath = path
			return nil
		},
	}

	repo := &mockMediaRepo{
		createFn: func(_ context.Context, _ *domain.Media, _ []uuid.UUID) (*domain.Media, error) {
			return nil, errors.New("database connection lost")
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "http://localhost:8080", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "test",
		File:        strings.NewReader("data"),
		FileName:    "photo.jpg",
		ContentType: "image/jpeg",
	})

	if err == nil {
		t.Fatal("expected error from DB failure, got nil")
	}
	if deletedPath != "a/b/c/test.jpg" {
		t.Errorf("expected file cleanup at 'a/b/c/test.jpg', got %q", deletedPath)
	}
}

// TestMediaService_Create_StorageFailure verifies that when file storage fails,
// no database operation is attempted.
func TestMediaService_Create_StorageFailure(t *testing.T) {
	dbCalled := false

	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) {
			return "", errors.New("disk full")
		},
	}

	repo := &mockMediaRepo{
		createFn: func(_ context.Context, _ *domain.Media, _ []uuid.UUID) (*domain.Media, error) {
			dbCalled = true
			return nil, nil
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "http://localhost:8080", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "test",
		File:        strings.NewReader("data"),
		FileName:    "photo.jpg",
		ContentType: "image/jpeg",
	})

	if err == nil {
		t.Fatal("expected storage error, got nil")
	}
	if dbCalled {
		t.Error("database should not be called when storage fails")
	}
}

// TestMediaService_Create_DuplicateTagIDs verifies that duplicate tag IDs in the
// input are deduplicated before being passed to the repository.
func TestMediaService_Create_DuplicateTagIDs(t *testing.T) {
	tagID := uuid.New()
	var capturedTagIDs []uuid.UUID

	storage := &mockStorage{
		saveFn: func(_ context.Context, _ io.Reader, _ string) (string, error) {
			return "path", nil
		},
	}

	repo := &mockMediaRepo{
		createFn: func(_ context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
			capturedTagIDs = tagIDs
			media.Tags = []domain.Tag{}
			return media, nil
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "http://localhost:8080", newTestLogger())

	_, err := svc.Create(context.Background(), CreateInput{
		Name:        "test",
		File:        strings.NewReader("data"),
		FileName:    "photo.jpg",
		TagIDs:      []uuid.UUID{tagID, tagID, tagID},
		ContentType: "image/jpeg",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedTagIDs) != 1 {
		t.Errorf("expected 1 deduplicated tag ID, got %d", len(capturedTagIDs))
	}
}

// TestMediaService_List_TagNamesSanitized verifies that each tag_name filter
// entry is passed through SanitizeName before reaching the repository — so
// whitespace variants match the stored canonical name.
func TestMediaService_List_TagNamesSanitized(t *testing.T) {
	var capturedFilter port.MediaFilter
	repo := &mockMediaRepo{
		listFn: func(_ context.Context, _ int, _ *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
			capturedFilter = filter
			return []*domain.Media{}, nil, nil
		},
	}
	svc := NewMediaService(repo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	_, _, err := svc.List(context.Background(), 50, "", port.MediaFilter{TagNames: []string{"  Mbappé  ", "Ligue 1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedFilter.TagNames) != 2 {
		t.Fatalf("expected 2 sanitized names, got %d", len(capturedFilter.TagNames))
	}
	if capturedFilter.TagNames[0] != "Mbappé" || capturedFilter.TagNames[1] != "Ligue 1" {
		t.Errorf("unexpected sanitized names: %v", capturedFilter.TagNames)
	}
}

// TestMediaService_List_TagNamesDeduped verifies that the AND count predicate
// requires deduplicated names — service-level dedupe collapses "  Mbappé  "
// and "Mbappé" so HAVING COUNT(DISTINCT) doesn't inflate.
func TestMediaService_List_TagNamesDeduped(t *testing.T) {
	var capturedFilter port.MediaFilter
	repo := &mockMediaRepo{
		listFn: func(_ context.Context, _ int, _ *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
			capturedFilter = filter
			return []*domain.Media{}, nil, nil
		},
	}
	svc := NewMediaService(repo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	_, _, err := svc.List(context.Background(), 50, "", port.MediaFilter{TagNames: []string{"Mbappé", "  Mbappé  ", "Mbappé"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedFilter.TagNames) != 1 {
		t.Errorf("expected 1 deduped name, got %d (%v)", len(capturedFilter.TagNames), capturedFilter.TagNames)
	}
}

// TestMediaService_List_TagIDsDeduped verifies the same dedupe for tag_id slice.
func TestMediaService_List_TagIDsDeduped(t *testing.T) {
	id := uuid.New()
	var capturedFilter port.MediaFilter
	repo := &mockMediaRepo{
		listFn: func(_ context.Context, _ int, _ *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
			capturedFilter = filter
			return []*domain.Media{}, nil, nil
		},
	}
	svc := NewMediaService(repo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	_, _, err := svc.List(context.Background(), 50, "", port.MediaFilter{TagIDs: []uuid.UUID{id, id, id}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedFilter.TagIDs) != 1 {
		t.Errorf("expected 1 deduped ID, got %d", len(capturedFilter.TagIDs))
	}
}

// TestMediaService_List_TagNameRejectsUnsafe verifies that an unsafe tag name
// (e.g. control character) is rejected with a validation error before the
// repository is called.
func TestMediaService_List_TagNameRejectsUnsafe(t *testing.T) {
	repoCalled := false
	repo := &mockMediaRepo{
		listFn: func(_ context.Context, _ int, _ *port.MediaCursor, _ port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
			repoCalled = true
			return nil, nil, nil
		},
	}
	svc := NewMediaService(repo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	_, _, err := svc.List(context.Background(), 50, "", port.MediaFilter{TagNames: []string{"name\x00with-null"}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
	if repoCalled {
		t.Error("repo should not be called when tag_name is invalid")
	}
}

// TestMediaService_List_FilterCap rejects more than maxFilterTags entries.
func TestMediaService_List_FilterCap(t *testing.T) {
	repo := &mockMediaRepo{
		listFn: func(_ context.Context, _ int, _ *port.MediaCursor, _ port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
			t.Error("repo should not be called when filter exceeds cap")
			return nil, nil, nil
		},
	}
	svc := NewMediaService(repo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	names := make([]string, maxFilterTags+1)
	for i := range names {
		names[i] = fmt.Sprintf("tag-%d", i)
	}
	_, _, err := svc.List(context.Background(), 50, "", port.MediaFilter{TagNames: names})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for over-cap filter, got %v", err)
	}
}

// TestMediaService_GetByID_NotFound verifies that requesting a non-existent media
// returns domain.ErrNotFound.
func TestMediaService_GetByID_NotFound(t *testing.T) {
	repo := &mockMediaRepo{
		getByIDFn: func(_ context.Context, _ uuid.UUID) (*domain.Media, error) {
			return nil, domain.ErrNotFound
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, nil, "", newTestLogger())

	_, err := svc.GetByID(context.Background(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestMediaService_Delete_Success verifies the happy path: DB deletion returns
// the file path, and the file is cleaned up from storage.
func TestMediaService_Delete_Success(t *testing.T) {
	var deletedPath string

	repo := &mockMediaRepo{
		deleteFn: func(_ context.Context, _ uuid.UUID) (string, error) {
			return "a/b/c/test.jpg", nil
		},
	}

	storage := &mockStorage{
		deleteFn: func(_ context.Context, path string) error {
			deletedPath = path
			return nil
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "", newTestLogger())
	err := svc.Delete(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deletedPath != "a/b/c/test.jpg" {
		t.Errorf("expected file cleanup at 'a/b/c/test.jpg', got %q", deletedPath)
	}
}

// TestMediaService_Delete_NotFound verifies that deleting a non-existent media
// returns domain.ErrNotFound.
func TestMediaService_Delete_NotFound(t *testing.T) {
	repo := &mockMediaRepo{
		deleteFn: func(_ context.Context, _ uuid.UUID) (string, error) {
			return "", domain.ErrNotFound
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, nil, "", newTestLogger())
	err := svc.Delete(context.Background(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestMediaService_Delete_FileCleanupFailure verifies that when the file deletion
// fails after a successful DB deletion, the Delete method still returns success.
// The DB is the source of truth — file cleanup failures are logged but non-fatal.
func TestMediaService_Delete_FileCleanupFailure(t *testing.T) {
	repo := &mockMediaRepo{
		deleteFn: func(_ context.Context, _ uuid.UUID) (string, error) {
			return "orphaned/file.jpg", nil
		},
	}

	storage := &mockStorage{
		deleteFn: func(_ context.Context, _ string) error {
			return errors.New("storage unreachable")
		},
	}

	svc := NewMediaService(repo, &mockTagRepo{}, storage, "", newTestLogger())
	err := svc.Delete(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Delete should succeed even when file cleanup fails, got: %v", err)
	}
}

// TestClassifyContentType_AllTypes verifies that all supported MIME types are
// correctly classified.
func TestClassifyContentType_AllTypes(t *testing.T) {
	tests := []struct {
		contentType string
		fileName    string
		wantType    domain.MediaType
		wantExt     string
		wantErr     bool
	}{
		{"image/jpeg", "photo.jpg", domain.MediaTypeImage, ".jpg", false},
		{"image/png", "photo.png", domain.MediaTypeImage, ".png", false},
		{"image/gif", "anim.gif", domain.MediaTypeImage, ".gif", false},
		{"image/webp", "photo.webp", domain.MediaTypeImage, ".webp", false},
		{"video/mp4", "video.mp4", domain.MediaTypeVideo, ".mp4", false},
		{"video/webm", "video.webm", domain.MediaTypeVideo, ".webm", false},
		// Fallback: octet-stream with known video extension
		{"application/octet-stream", "video.mov", domain.MediaTypeVideo, ".mov", false},
		{"application/octet-stream", "video.mkv", domain.MediaTypeVideo, ".mkv", false},
		// Rejected types
		{"application/pdf", "doc.pdf", "", "", true},
		{"text/html", "page.html", "", "", true},
		{"application/octet-stream", "malware.exe", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.contentType+"_"+tt.fileName, func(t *testing.T) {
			mt, ext, err := classifyContentType(tt.contentType, tt.fileName)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mt != tt.wantType {
				t.Errorf("expected type %q, got %q", tt.wantType, mt)
			}
			if ext != tt.wantExt {
				t.Errorf("expected ext %q, got %q", tt.wantExt, ext)
			}
		})
	}
}

// TestDeduplicateUUIDs verifies UUID deduplication preserves order and removes duplicates.
func TestDeduplicateUUIDs(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()

	result := deduplicateUUIDs([]uuid.UUID{a, b, a, c, b, c})
	if len(result) != 3 {
		t.Fatalf("expected 3 unique UUIDs, got %d", len(result))
	}
	if result[0] != a || result[1] != b || result[2] != c {
		t.Error("order not preserved after deduplication")
	}
}

// TestDeduplicateUUIDs_Empty verifies that deduplicating an empty slice returns
// the same empty slice (no panic, no allocation).
func TestDeduplicateUUIDs_Empty(t *testing.T) {
	result := deduplicateUUIDs([]uuid.UUID{})
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d items", len(result))
	}
}

// TestSanitizeFilename verifies that path traversal attempts are neutralized.
func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"photo.jpg", "photo.jpg"},
		{"../../etc/passwd", "passwd"},
		{"/tmp/malicious.sh", "malicious.sh"},
		{"C:\\Windows\\system32\\cmd.exe", "cmd.exe"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestMediaService_AttachTags_MergesAndDedupes verifies that explicit IDs
// and resolved-from-name IDs are merged into one deduped slice before
// reaching the repository — so a request that passes the same tag both
// ways doesn't create two AttachTags rows.
func TestMediaService_AttachTags_MergesAndDedupes(t *testing.T) {
	mediaID := uuid.New()
	sharedID := uuid.New()

	tagRepo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			return domain.Tag{ID: sharedID, Name: name}, false, nil
		},
	}

	var captured []uuid.UUID
	mediaRepo := &mockMediaRepo{
		attachTagsFn: func(_ context.Context, _ uuid.UUID, tagIDs []uuid.UUID) error {
			captured = tagIDs
			return nil
		},
		getByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Media, error) {
			return &domain.Media{ID: id, Tags: []domain.Tag{{ID: sharedID, Name: "x"}}}, nil
		},
	}

	svc := NewMediaService(mediaRepo, tagRepo, &mockStorage{}, "", newTestLogger())

	_, err := svc.AttachTags(context.Background(), mediaID,
		[]uuid.UUID{sharedID},   // by UUID
		[]string{"x"},           // same tag by name (resolves to sharedID)
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captured) != 1 || captured[0] != sharedID {
		t.Errorf("expected 1 deduped tag id, got %v", captured)
	}
}

// TestMediaService_AttachTags_Cap rejects batches above maxTagsPerMedia.
func TestMediaService_AttachTags_Cap(t *testing.T) {
	tooMany := make([]string, maxTagsPerMedia+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("t%d", i)
	}
	svc := NewMediaService(&mockMediaRepo{}, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())
	_, err := svc.AttachTags(context.Background(), uuid.New(), nil, tooMany)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
}

// TestMediaService_AttachTags_UnsafeName rejects unsafe tag names before
// touching either the tag repo or the media repo.
func TestMediaService_AttachTags_UnsafeName(t *testing.T) {
	createOrGetCalled := false
	attachCalled := false
	tagRepo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, _ string) (domain.Tag, bool, error) {
			createOrGetCalled = true
			return domain.Tag{}, false, nil
		},
	}
	mediaRepo := &mockMediaRepo{
		attachTagsFn: func(_ context.Context, _ uuid.UUID, _ []uuid.UUID) error {
			attachCalled = true
			return nil
		},
	}

	svc := NewMediaService(mediaRepo, tagRepo, &mockStorage{}, "", newTestLogger())
	_, err := svc.AttachTags(context.Background(), uuid.New(), nil, []string{"bad\x00name"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
	if createOrGetCalled || attachCalled {
		t.Error("repo should not be touched when sanitize fails")
	}
}

// TestMediaService_AttachTags_NotFound propagates the repo's ErrNotFound.
func TestMediaService_AttachTags_NotFound(t *testing.T) {
	mediaRepo := &mockMediaRepo{
		attachTagsFn: func(_ context.Context, _ uuid.UUID, _ []uuid.UUID) error {
			return domain.ErrNotFound
		},
	}
	svc := NewMediaService(mediaRepo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())

	_, err := svc.AttachTags(context.Background(), uuid.New(), []uuid.UUID{uuid.New()}, nil)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestMediaService_DetachTag_PassThrough verifies the service delegates and
// surfaces NotFound from the repo unchanged.
func TestMediaService_DetachTag_PassThrough(t *testing.T) {
	called := false
	mediaRepo := &mockMediaRepo{
		detachTagFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
			called = true
			return nil
		},
	}
	svc := NewMediaService(mediaRepo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())
	if err := svc.DetachTag(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("repo.DetachTag should be invoked")
	}
}

func TestMediaService_DetachTag_NotFound(t *testing.T) {
	mediaRepo := &mockMediaRepo{
		detachTagFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
			return domain.ErrNotFound
		},
	}
	svc := NewMediaService(mediaRepo, &mockTagRepo{}, &mockStorage{}, "", newTestLogger())
	if err := svc.DetachTag(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
