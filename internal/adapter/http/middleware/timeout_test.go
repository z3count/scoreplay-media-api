package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTimeout_SetsDeadline verifies that the middleware sets a context deadline
// on the request, and that handlers can observe it.
func TestTimeout_SetsDeadline(t *testing.T) {
	mw := Timeout(5 * time.Second)

	var hasDeadline bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !hasDeadline {
		t.Fatal("expected request context to have a deadline")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestTimeout_ContextCancelled verifies that a very short timeout causes the
// context to be cancelled when the handler takes too long.
func TestTimeout_ContextCancelled(t *testing.T) {
	mw := Timeout(1 * time.Millisecond)

	var ctxErr error
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow operation.
		time.Sleep(50 * time.Millisecond)
		ctxErr = r.Context().Err()
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if ctxErr == nil {
		t.Fatal("expected context to be cancelled after timeout, but Err() was nil")
	}
}

// TestTimeout_DeadlineValue verifies that the deadline is set to approximately
// the configured duration from now.
func TestTimeout_DeadlineValue(t *testing.T) {
	timeout := 10 * time.Second
	mw := Timeout(timeout)

	var deadline time.Time
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
		w.WriteHeader(http.StatusOK)
	}))

	before := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The deadline should be approximately `timeout` from now.
	expected := before.Add(timeout)
	if deadline.Before(expected.Add(-1*time.Second)) || deadline.After(expected.Add(1*time.Second)) {
		t.Fatalf("deadline %v is not within 1s of expected %v", deadline, expected)
	}
}
