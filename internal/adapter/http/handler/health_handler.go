package handler

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

// HealthHandler provides Kubernetes-style health check endpoints.
//
// Two endpoints are exposed:
//
//   - GET /healthz (liveness probe): returns 200 if the process is running.
//     If this fails, the orchestrator should restart the container.
//
//   - GET /readyz (readiness probe): returns 200 if the service is ready to
//     accept traffic (database is reachable). If this fails, the load balancer
//     should stop sending requests until the probe succeeds again.
//
// Both endpoints are intentionally placed OUTSIDE authentication middleware
// so that orchestrators and load balancers can probe them without credentials.
//
// Response format:
//
//	{"status": "ok"}      — 200 OK
//	{"status": "error", "details": "..."} — 503 Service Unavailable
type HealthHandler struct {
	db *sql.DB
}

// NewHealthHandler creates a HealthHandler that checks the given DB connection.
func NewHealthHandler(db *sql.DB) *HealthHandler {
	return &HealthHandler{db: db}
}

// Liveness handles GET /healthz.
//
// Always returns 200 OK if the process is alive. This is used by Kubernetes
// liveness probes — a failure would trigger a pod restart. We intentionally
// do NOT check the database here: a temporary DB outage should not cause a
// restart loop (the readiness probe handles traffic routing instead).
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readiness handles GET /readyz.
//
// Returns 200 OK if the service can serve requests (database is reachable).
// Returns 503 Service Unavailable if any dependency is down.
//
// Used by Kubernetes readiness probes and load balancers (ALB, nginx) to
// decide whether to route traffic to this instance. When a pod fails
// readiness, it is removed from the service endpoints but NOT restarted.
//
// The DB ping has a 2-second timeout to prevent the probe from hanging
// when the database is unreachable (network partition, connection exhaustion).
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.db.PingContext(ctx); err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":  "error",
			"details": "database unreachable",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
