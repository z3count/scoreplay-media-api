package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// mockTagRepo is a test double for port.TagRepository.
// It records calls and returns preconfigured responses, allowing us to test
// TagService logic in isolation without a real database.
type mockTagRepo struct {
	createOrGetFn func(ctx context.Context, name string) (domain.Tag, bool, error)
	listFn        func(ctx context.Context, limit int, cursor *port.TagCursor) ([]domain.Tag, *port.TagCursor, error)
	renameFn      func(ctx context.Context, id uuid.UUID, name string) (domain.Tag, error)
	deleteFn      func(ctx context.Context, id uuid.UUID) error
}

func (m *mockTagRepo) CreateOrGet(ctx context.Context, name string) (domain.Tag, bool, error) {
	if m.createOrGetFn == nil {
		return domain.Tag{}, false, nil
	}
	return m.createOrGetFn(ctx, name)
}

func (m *mockTagRepo) List(ctx context.Context, limit int, cursor *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
	if m.listFn == nil {
		return nil, nil, nil
	}
	return m.listFn(ctx, limit, cursor)
}

func (m *mockTagRepo) Rename(ctx context.Context, id uuid.UUID, name string) (domain.Tag, error) {
	if m.renameFn == nil {
		return domain.Tag{}, nil
	}
	return m.renameFn(ctx, id, name)
}

func (m *mockTagRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if m.deleteFn == nil {
		return nil
	}
	return m.deleteFn(ctx, id)
}

// TestTagService_Create_Success verifies that a valid tag name is trimmed and
// passed to the repository, and the created flag is propagated correctly.
func TestTagService_Create_Success(t *testing.T) {
	var capturedName string
	repo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			capturedName = name
			return domain.Tag{Name: name}, true, nil
		},
	}
	svc := NewTagService(repo)

	tag, created, err := svc.Create(context.Background(), "  Mbappé  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !created {
		t.Error("expected created=true")
	}
	if capturedName != "Mbappé" {
		t.Errorf("expected trimmed name 'Mbappé', got %q", capturedName)
	}
	if tag.Name != "Mbappé" {
		t.Errorf("expected tag name 'Mbappé', got %q", tag.Name)
	}
}

// TestTagService_Create_EmptyName verifies that empty or whitespace-only names
// are rejected with a validation error.
func TestTagService_Create_EmptyName(t *testing.T) {
	repo := &mockTagRepo{}
	svc := NewTagService(repo)

	tests := []struct {
		name  string
		input string
	}{
		{"empty string", ""},
		{"whitespace only", "   "},
		{"tabs and spaces", "\t  \t"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := svc.Create(context.Background(), tt.input)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !isValidationError(err) {
				t.Errorf("expected validation error, got: %v", err)
			}
		})
	}
}

// TestTagService_Create_TooLong verifies that names exceeding 255 characters
// are rejected with a validation error.
func TestTagService_Create_TooLong(t *testing.T) {
	repo := &mockTagRepo{}
	svc := NewTagService(repo)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	_, _, err := svc.Create(context.Background(), string(longName))
	if err == nil {
		t.Fatal("expected validation error for long name, got nil")
	}
	if !isValidationError(err) {
		t.Errorf("expected validation error, got: %v", err)
	}
}

// TestTagService_Create_ExactMaxLength verifies that a name with exactly 255
// characters is accepted (boundary test).
func TestTagService_Create_ExactMaxLength(t *testing.T) {
	repo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			return domain.Tag{Name: name}, true, nil
		},
	}
	svc := NewTagService(repo)

	exactName := make([]byte, 255)
	for i := range exactName {
		exactName[i] = 'a'
	}

	_, _, err := svc.Create(context.Background(), string(exactName))
	if err != nil {
		t.Fatalf("expected success for 255-char name, got: %v", err)
	}
}

// TestTagService_Create_Idempotent verifies that creating a tag that already
// exists returns created=false (idempotent behavior).
func TestTagService_Create_Idempotent(t *testing.T) {
	repo := &mockTagRepo{
		createOrGetFn: func(_ context.Context, name string) (domain.Tag, bool, error) {
			return domain.Tag{Name: name}, false, nil
		},
	}
	svc := NewTagService(repo)

	_, created, err := svc.Create(context.Background(), "existing-tag")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created {
		t.Error("expected created=false for existing tag")
	}
}

// TestTagService_List_PaginationDefaults verifies that limit=0 falls back to
// the default and that an empty cursor reaches the repo as nil ("first page").
func TestTagService_List_PaginationDefaults(t *testing.T) {
	var capturedLimit int
	var capturedCursor *port.TagCursor
	repo := &mockTagRepo{
		listFn: func(_ context.Context, limit int, cursor *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
			capturedLimit = limit
			capturedCursor = cursor
			return []domain.Tag{}, nil, nil
		},
	}
	svc := NewTagService(repo)

	_, next, err := svc.List(context.Background(), 0, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedLimit != 50 {
		t.Errorf("expected default limit 50, got %d", capturedLimit)
	}
	if capturedCursor != nil {
		t.Errorf("expected nil cursor for first page, got %+v", capturedCursor)
	}
	if next != "" {
		t.Errorf("expected empty next cursor when repo returns nil, got %q", next)
	}
}

// TestTagService_List_LimitClamping verifies that limit is capped at 100.
func TestTagService_List_LimitClamping(t *testing.T) {
	var capturedLimit int
	repo := &mockTagRepo{
		listFn: func(_ context.Context, limit int, _ *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
			capturedLimit = limit
			return []domain.Tag{}, nil, nil
		},
	}
	svc := NewTagService(repo)

	_, _, err := svc.List(context.Background(), 500, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedLimit != 100 {
		t.Errorf("expected capped limit 100, got %d", capturedLimit)
	}
}

// TestTagService_List_InvalidCursor verifies that a malformed cursor is
// reported as a validation error (no repo call).
func TestTagService_List_InvalidCursor(t *testing.T) {
	repoCalled := false
	repo := &mockTagRepo{
		listFn: func(_ context.Context, _ int, _ *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
			repoCalled = true
			return nil, nil, nil
		},
	}
	svc := NewTagService(repo)

	_, _, err := svc.List(context.Background(), 50, "!!!not-base64!!!")
	if err == nil {
		t.Fatal("expected error for malformed cursor")
	}
	if !isValidationError(err) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
	if repoCalled {
		t.Error("repo should not be called when the cursor is invalid")
	}
}

// isValidationError checks if an error wraps domain.ErrValidation.
func isValidationError(err error) bool {
	return err != nil && containsError(err, domain.ErrValidation)
}

func containsError(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// TestTagService_Rename_Success verifies that the new name is sanitized
// before reaching the repository.
func TestTagService_Rename_Success(t *testing.T) {
	var capturedName string
	repo := &mockTagRepo{
		renameFn: func(_ context.Context, id uuid.UUID, name string) (domain.Tag, error) {
			capturedName = name
			return domain.Tag{ID: id, Name: name}, nil
		},
	}
	svc := NewTagService(repo)

	id := uuid.New()
	tag, err := svc.Rename(context.Background(), id, "  Mbappé  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedName != "Mbappé" {
		t.Errorf("expected sanitized name 'Mbappé', got %q", capturedName)
	}
	if tag.ID != id {
		t.Errorf("expected returned tag id to match input")
	}
}

// TestTagService_Rename_EmptyName ensures an empty/whitespace name is
// rejected before the repository is touched.
func TestTagService_Rename_EmptyName(t *testing.T) {
	repoCalled := false
	repo := &mockTagRepo{
		renameFn: func(_ context.Context, _ uuid.UUID, _ string) (domain.Tag, error) {
			repoCalled = true
			return domain.Tag{}, nil
		},
	}
	svc := NewTagService(repo)

	_, err := svc.Rename(context.Background(), uuid.New(), "   ")
	if !isValidationError(err) {
		t.Errorf("expected ErrValidation, got %v", err)
	}
	if repoCalled {
		t.Error("repo Rename should not be called for invalid name")
	}
}

// TestTagService_Rename_NotFound propagates ErrNotFound from the repo.
func TestTagService_Rename_NotFound(t *testing.T) {
	repo := &mockTagRepo{
		renameFn: func(_ context.Context, _ uuid.UUID, _ string) (domain.Tag, error) {
			return domain.Tag{}, domain.ErrNotFound
		},
	}
	svc := NewTagService(repo)

	_, err := svc.Rename(context.Background(), uuid.New(), "valid")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestTagService_Rename_Conflict propagates ErrConflict from the repo.
func TestTagService_Rename_Conflict(t *testing.T) {
	repo := &mockTagRepo{
		renameFn: func(_ context.Context, _ uuid.UUID, _ string) (domain.Tag, error) {
			return domain.Tag{}, domain.ErrConflict
		},
	}
	svc := NewTagService(repo)

	_, err := svc.Rename(context.Background(), uuid.New(), "taken")
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}

// TestTagService_Delete_Success verifies straight pass-through.
func TestTagService_Delete_Success(t *testing.T) {
	called := false
	repo := &mockTagRepo{
		deleteFn: func(_ context.Context, _ uuid.UUID) error {
			called = true
			return nil
		},
	}
	svc := NewTagService(repo)

	if err := svc.Delete(context.Background(), uuid.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected repo.Delete to be invoked")
	}
}

// TestTagService_Delete_NotFound propagates ErrNotFound.
func TestTagService_Delete_NotFound(t *testing.T) {
	repo := &mockTagRepo{
		deleteFn: func(_ context.Context, _ uuid.UUID) error {
			return domain.ErrNotFound
		},
	}
	svc := NewTagService(repo)

	if err := svc.Delete(context.Background(), uuid.New()); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
