package port

import (
	"context"
	"io"
	"time"
)

// FileStorage defines the contract for file storage operations.
//
// This interface abstracts the physical storage backend, enabling the service
// layer to store and retrieve files without knowing whether they live on a
// local disk, Amazon S3, Google Cloud Storage, or any other backend.
//
// For this project, we implement LocalStorage (hash-sharded local disk).
// In production, an S3Storage implementation would be a drop-in replacement
// with no changes to the service layer.
//
// Implementations must:
//   - Be safe for concurrent use from multiple goroutines.
//   - Generate unique filenames to prevent collisions (UUID-based).
//   - Never use client-provided filenames for storage paths (path traversal prevention).
//   - Stream data from the reader without buffering the entire file in memory.
type FileStorage interface {
	// Save streams the content from reader to persistent storage and returns
	// the relative storage path (e.g., "4/f/3/a1b2c3d4.jpg" for local storage,
	// or an S3 object key for cloud storage).
	//
	// Parameters:
	//   - ctx: request context for cancellation/timeout propagation.
	//   - reader: the file content to store. The caller is responsible for closing
	//     the reader after Save returns.
	//   - ext: the file extension including the dot (e.g., ".jpg", ".mp4").
	//     This is derived from content sniffing, not from the client filename.
	//
	// The returned path is what gets stored in the database. It is relative to
	// the storage root — the handler or URL builder is responsible for constructing
	// the full public URL.
	//
	// If the underlying storage is full or an I/O error occurs, the implementation
	// must clean up any partially written file before returning the error.
	Save(ctx context.Context, reader io.Reader, ext string) (path string, err error)

	// Delete removes a previously stored file by its relative path.
	//
	// This is called as a compensating action when a database insert fails
	// after the file has already been written. It implements the "undo" half
	// of the manual rollback pattern for maintaining file/DB consistency.
	//
	// If the file does not exist, Delete should return nil (idempotent).
	// Errors are logged but generally not propagated to the end user (the
	// original DB error takes precedence).
	Delete(ctx context.Context, path string) error

	// URL returns the public-facing URL for a stored file, given its relative path.
	//
	// For local storage: baseURL + "/uploads/" + path
	// For S3 storage:    CDN URL when S3_CDN_URL is set, else the bucket URL
	//
	// The baseURL parameter allows constructing absolute URLs suitable for
	// inclusion in API responses.
	URL(baseURL, path string) string

	// URLWithExpiry returns a short-lived URL for reading the file at path.
	//
	// Intended for private-tier media: the URL embeds a signature and an
	// expiry, so anyone who obtains it can read the file only until ttl
	// elapses. After that, the URL returns an auth error from the backend.
	//
	// Behaviour per backend:
	//   - s3:    real presigned GET URL signed with the app's credentials
	//            (PresignClient.PresignGetObject). The bucket can be fully
	//            private; the URL alone authorises a read.
	//   - local: returns the same URL as URL(). The local backend has no
	//            cryptographic notion of "expiry" — it's intended for dev
	//            only, so ttl is advisory and ignored.
	//
	// Callers must not assume that local URLs actually expire. Production
	// deployments that require private media must use the S3 backend.
	URLWithExpiry(ctx context.Context, baseURL, path string, ttl time.Duration) (string, error)
}
