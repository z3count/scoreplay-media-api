package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// mockIdempotencyStore is an in-memory implementation for testing.
type mockIdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]storedEntry
}

type storedEntry struct {
	statusCode int
	body       []byte
}

func newMockStore() *mockIdempotencyStore {
	return &mockIdempotencyStore{entries: make(map[string]storedEntry)}
}

func (m *mockIdempotencyStore) Get(_ context.Context, key string) (int, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return 0, nil, false, nil
	}
	return entry.statusCode, entry.body, true, nil
}

func (m *mockIdempotencyStore) Set(_ context.Context, key string, statusCode int, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; !ok {
		// Mimic ON CONFLICT DO NOTHING — first writer wins.
		cp := make([]byte, len(body))
		copy(cp, body)
		m.entries[key] = storedEntry{statusCode: statusCode, body: cp}
	}
	return nil
}

func (m *mockIdempotencyStore) Cleanup(_ context.Context) error {
	return nil
}

// callCounter is a test handler that counts invocations.
type callCounter struct {
	mu    sync.Mutex
	count int
}

func (c *callCounter) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": "test-123"})
}

func (c *callCounter) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// TestIdempotency_NoHeader passes through without caching.
func TestIdempotency_NoHeader(t *testing.T) {
	store := newMockStore()
	counter := &callCounter{}
	handler := Idempotency(store)(counter)

	// Two requests without header → both executed.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/media", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d", w.Code)
		}
	}

	if counter.getCount() != 2 {
		t.Errorf("expected handler called 2 times, got %d", counter.getCount())
	}
}

// TestIdempotency_FirstCallExecutesHandler verifies the handler is called on
// the first request with a given key.
func TestIdempotency_FirstCallExecutesHandler(t *testing.T) {
	store := newMockStore()
	counter := &callCounter{}
	handler := Idempotency(store)(counter)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/media", nil)
	req.Header.Set("Idempotency-Key", "key-1")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	if counter.getCount() != 1 {
		t.Errorf("expected handler called once, got %d", counter.getCount())
	}
	// Verify no replayed header on first call.
	if w.Header().Get("Idempotency-Replayed") != "" {
		t.Error("first call should not have Idempotency-Replayed header")
	}
}

// TestIdempotency_RetryReplaysResponse verifies that a retry with the same
// key replays the cached response without re-executing the handler.
func TestIdempotency_RetryReplaysResponse(t *testing.T) {
	store := newMockStore()
	counter := &callCounter{}
	handler := Idempotency(store)(counter)

	// First call.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/media", nil)
	req1.Header.Set("Idempotency-Key", "key-2")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	// Second call (retry) with same key.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/media", nil)
	req2.Header.Set("Idempotency-Key", "key-2")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	// Handler should only be called once.
	if counter.getCount() != 1 {
		t.Errorf("expected handler called once, got %d", counter.getCount())
	}

	// Both responses should be identical.
	if w2.Code != w1.Code {
		t.Errorf("expected same status %d, got %d", w1.Code, w2.Code)
	}
	if w2.Body.String() != w1.Body.String() {
		t.Errorf("expected same body %q, got %q", w1.Body.String(), w2.Body.String())
	}

	// Retry should have the replayed header.
	if w2.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("retry should have Idempotency-Replayed: true header")
	}
}

// TestIdempotency_DifferentKeysExecuteSeparately verifies that different
// keys result in separate handler executions.
func TestIdempotency_DifferentKeysExecuteSeparately(t *testing.T) {
	store := newMockStore()
	counter := &callCounter{}
	handler := Idempotency(store)(counter)

	for _, key := range []string{"key-a", "key-b", "key-c"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/media", nil)
		req.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("key %s: expected 201, got %d", key, w.Code)
		}
	}

	if counter.getCount() != 3 {
		t.Errorf("expected 3 handler calls, got %d", counter.getCount())
	}
}

// TestIdempotency_ErrorNotCached verifies that error responses (4xx, 5xx)
// are NOT cached, allowing the client to fix the request and retry.
func TestIdempotency_ErrorNotCached(t *testing.T) {
	store := newMockStore()
	calls := 0

	errorHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad"}`))
	})

	handler := Idempotency(store)(errorHandler)

	// First call → 400.
	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.Header.Set("Idempotency-Key", "err-key")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)

	// Second call → handler should execute again (error was not cached).
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Idempotency-Key", "err-key")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if calls != 2 {
		t.Errorf("expected handler called twice (error not cached), got %d", calls)
	}
}

// TestIdempotency_MultipleRetries verifies that the response remains
// consistent across many retries.
func TestIdempotency_MultipleRetries(t *testing.T) {
	store := newMockStore()
	counter := &callCounter{}
	handler := Idempotency(store)(counter)

	// First call.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "multi-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	firstBody := w.Body.String()

	// 10 retries.
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Idempotency-Key", "multi-key")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			t.Fatalf("retry %d: expected 201, got %d", i, w.Code)
		}
		if w.Body.String() != firstBody {
			t.Fatalf("retry %d: body mismatch", i)
		}
	}

	if counter.getCount() != 1 {
		t.Errorf("expected handler called once, got %d", counter.getCount())
	}
}
