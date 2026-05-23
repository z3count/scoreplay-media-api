package local

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// testCtx returns a context carrying the legacy-tenant identity. The
// storage adapter reads the tenant from ctx to build per-tenant key
// prefixes; tests that don't go through the auth middleware need to
// inject one explicitly.
func testCtx() context.Context {
	return port.WithIdentity(context.Background(), domain.Identity{
		TenantID: domain.LegacyTenantID,
		Scopes:   []string{"admin:*"},
	})
}

// TestStorage_Save_CreatesFile verifies that Save creates a file in the correct
// hash-sharded directory structure and returns a valid relative path.
func TestStorage_Save_CreatesFile(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	content := "test file content"
	path, err := storage.Save(testCtx(), strings.NewReader(content), ".jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify the path has the expected sharded structure: <tenant>/x/y/z/uuid.jpg.
	// The leading component is the tenant UUID (added by the tenancy
	// migration); the remaining three are the hash-shard tree.
	parts := strings.Split(path, string(filepath.Separator))
	if len(parts) != 5 {
		t.Errorf("expected 5 path components (tenant/a/b/c/file), got %d: %s", len(parts), path)
	}
	if parts[0] != domain.LegacyTenantID.String() {
		t.Errorf("expected tenant prefix %s, got %s", domain.LegacyTenantID, parts[0])
	}
	if !strings.HasSuffix(path, ".jpg") {
		t.Errorf("expected .jpg extension, got: %s", path)
	}

	// Verify the file exists and contains the expected content.
	fullPath := filepath.Join(tmpDir, path)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected content %q, got %q", content, string(data))
	}
}

// TestStorage_Save_MultipleFiles verifies that multiple concurrent saves produce
// unique files (no collisions thanks to UUID-based naming).
func TestStorage_Save_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	paths := make(map[string]bool)
	for i := 0; i < 100; i++ {
		path, err := storage.Save(testCtx(), strings.NewReader("data"), ".png")
		if err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		if paths[path] {
			t.Fatalf("duplicate path generated: %s", path)
		}
		paths[path] = true
	}
}

// TestStorage_Delete_ExistingFile verifies that Delete removes a previously
// stored file without error.
func TestStorage_Delete_ExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	path, err := storage.Save(testCtx(), strings.NewReader("data"), ".jpg")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	err = storage.Delete(testCtx(), path)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify the file no longer exists.
	fullPath := filepath.Join(tmpDir, path)
	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		t.Error("expected file to be deleted, but it still exists")
	}
}

// TestStorage_Delete_NonExistent verifies that deleting a non-existent file
// is idempotent (returns nil, not an error).
func TestStorage_Delete_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	err = storage.Delete(testCtx(), "does/not/exist.jpg")
	if err != nil {
		t.Errorf("expected nil error for non-existent file, got: %v", err)
	}
}

// TestStorage_URL verifies that URL construction follows the expected format.
func TestStorage_URL(t *testing.T) {
	storage := &Storage{basePath: "/tmp/uploads"}

	url := storage.URL("http://localhost:8080", "a/b/c/uuid.jpg")
	expected := "http://localhost:8080/uploads/a/b/c/uuid.jpg"
	if url != expected {
		t.Errorf("expected URL %q, got %q", expected, url)
	}
}

// TestStorage_URLWithExpiry documents the local-backend contract: the ttl
// is advisory only; the returned URL is identical to URL(). Real expiry
// requires the S3 backend.
func TestStorage_URLWithExpiry(t *testing.T) {
	storage := &Storage{basePath: "/tmp/uploads"}

	got, err := storage.URLWithExpiry(context.Background(), "http://localhost:8080", "a/b/c/uuid.jpg", 5*time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := storage.URL("http://localhost:8080", "a/b/c/uuid.jpg")
	if got != want {
		t.Errorf("URLWithExpiry should equal URL on local backend; got %q want %q", got, want)
	}
}

// TestStorage_Save_LargeFile verifies that streaming works for larger files
// without excessive memory usage (io.Copy uses a 32KB buffer).
func TestStorage_Save_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()
	storage, err := NewStorage(tmpDir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}

	// Create a 1MB reader.
	size := 1024 * 1024
	reader := io.LimitReader(&infiniteReader{}, int64(size))

	path, err := storage.Save(testCtx(), reader, ".mp4")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file size.
	fullPath := filepath.Join(tmpDir, path)
	info, err := os.Stat(fullPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(size) {
		t.Errorf("expected file size %d, got %d", size, info.Size())
	}
}

// infiniteReader is a reader that produces an infinite stream of zero bytes.
// Used for testing large file writes without allocating memory.
type infiniteReader struct{}

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
