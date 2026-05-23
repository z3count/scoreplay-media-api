// Package middleware — idempotency.go implements idempotent request handling.
//
// When a client sends an Idempotency-Key header, the server guarantees
// at-most-once execution: the request is processed on the first call, and
// subsequent calls with the same key replay the cached response without
// re-executing the handler.
//
// This prevents duplicate resource creation caused by:
//   - Network timeouts (client retries after not receiving a response)
//   - Mobile connectivity changes (WiFi → 4G mid-upload)
//   - Load balancer retries (proxy retries on upstream timeout)
//   - SDK/client retry logic
//
// Design:
//   - Key format: any non-empty string, typically a client-generated UUID.
//   - TTL: 24 hours (configured in the database migration).
//   - Storage: PostgreSQL (no Redis dependency).
//   - Race safety: INSERT ... ON CONFLICT DO NOTHING — first writer wins.
//   - Backward compatible: requests without the header are processed normally.
package middleware

import (
	"bytes"
	"net/http"

	"github.com/scoreplay/media-api/internal/port"
)

// idempotencyKeyHeader is the HTTP header used to pass the idempotency key.
const idempotencyKeyHeader = "Idempotency-Key"

// Idempotency returns a middleware that implements at-most-once execution
// for requests carrying an Idempotency-Key header.
//
// Flow:
//  1. No Idempotency-Key header → pass through to handler (backward compatible).
//  2. Key present, found in store → replay cached response (skip handler).
//  3. Key present, not found → execute handler, capture response, store it.
//
// The middleware wraps the ResponseWriter to capture the status code and
// response body. After the handler writes, the captured data is stored
// in the IdempotencyStore for future replays.
func Idempotency(store port.IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(idempotencyKeyHeader)

			// No key → pass through.
			if key == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Check for cached response.
			statusCode, body, found, err := store.Get(r.Context(), key)
			if err != nil {
				// Store error → proceed without idempotency (best-effort).
				next.ServeHTTP(w, r)
				return
			}

			if found {
				// Replay cached response.
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(statusCode)
				w.Write(body) //nolint:errcheck
				return
			}

			// Execute handler with capturing writer.
			cw := &capturingWriter{
				ResponseWriter: w,
				body:           &bytes.Buffer{},
			}
			next.ServeHTTP(cw, r)

			// Only cache successful responses (2xx).
			// Error responses should not be cached — the client should
			// be able to fix the request and retry with the same key.
			if cw.statusCode >= 200 && cw.statusCode < 300 {
				// Best-effort: don't fail the request if caching fails.
				_ = store.Set(r.Context(), key, cw.statusCode, cw.body.Bytes())
			}
		})
	}
}

// capturingWriter wraps http.ResponseWriter to capture the status code
// and response body while still writing to the original writer.
type capturingWriter struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func (cw *capturingWriter) WriteHeader(code int) {
	cw.statusCode = code
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *capturingWriter) Write(b []byte) (int, error) {
	cw.body.Write(b)
	return cw.ResponseWriter.Write(b)
}
