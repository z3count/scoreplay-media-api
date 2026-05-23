package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/lib/pq"
)

func TestHealthHandler_Liveness(t *testing.T) {
	h := NewHealthHandler(nil) // liveness doesn't need a real DB
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	h.Liveness(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status 'ok', got %q", body["status"])
	}
}

func TestHealthHandler_Readiness_NoDB(t *testing.T) {
	// Readiness with a broken DB connection should return 503.
	brokenDB, _ := sql.Open("postgres", "postgres://invalid:invalid@localhost:1/nope?sslmode=disable&connect_timeout=1")
	h := NewHealthHandler(brokenDB)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	h.Readiness(rec, req)

	// Should fail since the DB is unreachable.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}
