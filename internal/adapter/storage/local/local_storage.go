// Package local implements the port.FileStorage interface using the local
// filesystem with a hash-based directory sharding strategy.
//
// Why hash-sharded directories?
// A flat directory with 100k+ files causes severe performance degradation:
//   - readdir() system calls become O(n) on most filesystems (ext4, xfs).
//   - inode lookup degrades as the directory's hash table grows.
//   - Some tools (ls, find, rsync) break or hang on very large directories.
//
// Our sharding strategy uses the first 3 characters of a UUID to create a
// 3-level directory tree: "4/f/3e/<uuid>.<ext>". With 16 hex chars per level,
// this gives us 16 × 16 × 16 = 4096 possible leaf directories, each holding
// a manageable number of files even with millions of total uploads.
//
// This is the same strategy used by Git for its object store.
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/scoreplay/media-api/internal/port"
)

// Storage implements port.FileStorage using the local filesystem.
//
// Files are organized in a hash-sharded directory tree under a configurable
// base path. The sharding ensures no single directory contains more than a
// few thousand files, maintaining filesystem performance at scale.
//
// Thread safety: this implementation is safe for concurrent use. Each call
// to Save generates a unique UUID-based filename, so concurrent uploads
// never collide.
type Storage struct {
	basePath string // absolute path to the root uploads directory
}

// NewStorage creates a new local file storage rooted at basePath.
//
// The basePath directory is created if it does not exist (with 0750 permissions).
// This matches typical production setups where the upload directory is created
// once during deployment.
//
// Returns an error if the directory cannot be created (e.g., permission denied,
// read-only filesystem).
func NewStorage(basePath string) (*Storage, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("resolve upload path: %w", err)
	}

	if err := os.MkdirAll(absPath, 0750); err != nil {
		return nil, fmt.Errorf("create upload directory: %w", err)
	}

	return &Storage{basePath: absPath}, nil
}

// Save streams file content from reader to a hash-sharded path on the local
// filesystem and returns the relative storage path.
//
// The process:
//   1. Generate a UUID for the filename (never reuse client filenames).
//   2. Compute the sharded directory: first 3 chars of UUID → 3 nested dirs.
//   3. Create the directory tree if it doesn't exist.
//   4. Create the destination file.
//   5. Stream data from reader to file using io.Copy (no buffering in memory).
//   6. Sync the file to disk (fsync) to ensure durability.
//
// If any step fails after the file has been partially written, the partial file
// is removed to prevent orphaned data.
//
// Parameters:
//   - ctx: context for cancellation. If cancelled during io.Copy, the partial
//     file is cleaned up.
//   - reader: source of file data. The caller manages its lifecycle.
//   - ext: file extension with dot (e.g., ".jpg"). Derived from content sniffing.
//
// Returns the relative path from the storage root (e.g., "4/f/3/uuid.jpg").
func (s *Storage) Save(ctx context.Context, reader io.Reader, ext string) (string, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return "", err
	}

	// Generate a UUID-based filename.
	id := uuid.New().String()

	// Build the sharded directory path: <tenant_id>/4/f/3/
	// Using first 3 characters of the UUID hex string. Tenant id prefix
	// gives one-bucket / one-tree-per-tenant on disk; cross-tenant
	// browsing would require predicting both the tenant UUID and the
	// media UUID.
	dirPath := filepath.Join(tenantID.String(), string(id[0]), string(id[1]), string(id[2]))
	fullDir := filepath.Join(s.basePath, dirPath)

	if err := os.MkdirAll(fullDir, 0750); err != nil {
		return "", fmt.Errorf("create shard directory %s: %w", dirPath, err)
	}

	// Build the full file path: 4/f/3/a1b2c3d4-...-uuid.jpg
	fileName := id + ext
	relativePath := filepath.Join(dirPath, fileName)
	fullPath := filepath.Join(s.basePath, relativePath)

	// Create the destination file.
	dst, err := os.Create(fullPath)
	if err != nil {
		return "", fmt.Errorf("create file %s: %w", relativePath, err)
	}

	// Use a closure to handle cleanup on error.
	var writeErr error
	defer func() {
		dst.Close()
		if writeErr != nil {
			// Clean up partial file on error.
			os.Remove(fullPath) //nolint:errcheck // best-effort cleanup
		}
	}()

	// Stream data from reader to file.
	// io.Copy uses a 32KB buffer internally — the entire file is never in memory.
	_, writeErr = io.Copy(dst, reader)
	if writeErr != nil {
		return "", fmt.Errorf("write file %s: %w", relativePath, writeErr)
	}

	// Sync to disk to ensure data is persisted even if the system crashes
	// immediately after this call returns.
	if writeErr = dst.Sync(); writeErr != nil {
		return "", fmt.Errorf("sync file %s: %w", relativePath, writeErr)
	}

	return relativePath, nil
}

// Delete removes a file at the given relative path from the storage root.
//
// This method is called as a compensating action when a database insert fails
// after the file has been successfully written. It ensures file-database
// consistency by undoing the file write.
//
// Behavior:
//   - If the file exists, it is removed.
//   - If the file does not exist (already deleted, or never fully written),
//     the method returns nil (idempotent delete).
//   - Directory cleanup is not performed: empty shard directories are harmless
//     and will be reused by future uploads.
//
// Errors from os.Remove (other than "not found") are returned to the caller
// for logging purposes, but typically should not be propagated to the end user
// since the primary error (DB failure) takes precedence.
func (s *Storage) Delete(ctx context.Context, path string) error {
	fullPath := filepath.Join(s.basePath, path)

	if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete file %s: %w", path, err)
	}

	return nil
}

// URL constructs the public-facing URL for a stored file.
//
// For local storage, the URL is built by appending the upload prefix and
// relative path to the base URL. Example:
//   baseURL="http://localhost:8080", path="4/f/3/uuid.jpg"
//   → "http://localhost:8080/uploads/4/f/3/uuid.jpg"
//
// In a production environment with S3 storage, this method would instead
// generate a pre-signed URL or return a CDN URL. The interface abstraction
// allows this change without modifying any calling code.
func (s *Storage) URL(baseURL, path string) string {
	return baseURL + "/uploads/" + path
}

// URLWithExpiry returns the same URL as URL — the local filesystem backend
// has no cryptographic notion of "expiry". The ttl is accepted to satisfy
// port.FileStorage but is otherwise advisory: anyone who can hit the
// /uploads/* endpoint can read the file indefinitely.
//
// This is intentional. The local backend is for development and single-
// instance deployments where access control lives at the network layer
// (private subnet, reverse-proxy auth, etc.). Production deployments that
// need real per-URL expiry must use the S3 backend, whose implementation
// returns a presigned URL with a server-enforced TTL.
func (s *Storage) URLWithExpiry(_ context.Context, baseURL, path string, _ time.Duration) (string, error) {
	return s.URL(baseURL, path), nil
}
