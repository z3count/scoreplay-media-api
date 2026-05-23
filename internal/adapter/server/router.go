// Package server wires up the HTTP router with all handlers and middleware.
//
// This is the composition root for the HTTP layer: it creates the chi router,
// registers middleware in the correct order, mounts all route groups, and
// sets up the static file server for uploaded media.
package server

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/scoreplay/media-api/internal/adapter/http/handler"
	appmw "github.com/scoreplay/media-api/internal/adapter/http/middleware"
	"github.com/scoreplay/media-api/internal/port"
)

// NewRouter creates and configures the chi router with all routes and middleware.
//
// Middleware execution order (outermost first):
//  1. RequestID — assigns/preserves a unique request ID for tracing.
//  2. RealIP — extracts the real client IP from X-Forwarded-For headers
//     (important when running behind a reverse proxy / load balancer).
//  3. Recovery — catches panics and returns 500 instead of crashing the server.
//  4. Logger — logs every request with method, path, status, and duration.
//  5. Compress — gzip-compresses JSON responses for bandwidth savings.
//  6. APIKeyAuth — enforces API key authentication on /api/v1 routes.
//
// Route groups:
//   - /healthz       → liveness probe (no auth)
//   - /readyz        → readiness probe (no auth)
//   - /metrics       → Prometheus metrics (no auth)
//   - /api/v1/tags   → tag CRUD
//   - /api/v1/media  → media CRUD
//   - /uploads/      → static file server for uploaded media
//
// Parameters:
//   - healthHandler: handler for health check endpoints
//   - tagHandler: handler for tag endpoints
//   - mediaHandler: handler for media endpoints
//   - uploadDir: filesystem path to serve uploaded files from
//   - apiKey: API key for authentication (empty = auth disabled)
//   - corsOrigins: allowed CORS origins (empty = no cross-origin allowed)
//   - requestTimeout: context deadline for each request
//   - rateLimitRPS: sustained requests per second per IP
//   - rateLimitBurst: maximum burst size
//   - idempotencyStore: store for idempotent request handling (nil = disabled)
//   - logger: structured logger for the logging middleware
func NewRouter(
	healthHandler *handler.HealthHandler,
	tagHandler *handler.TagHandler,
	mediaHandler *handler.MediaHandler,
	uploadDir string,
	apiKey string,
	corsOrigins []string,
	requestTimeout time.Duration,
	rateLimitRPS float64,
	rateLimitBurst int,
	idempotencyStore port.IdempotencyStore,
	logger *slog.Logger,
) http.Handler {
	r := chi.NewRouter()

	// Global middleware stack.
	metrics := appmw.NewMetrics()
	r.Use(appmw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(appmw.Recovery(logger))
	r.Use(appmw.Timeout(requestTimeout))
	r.Use(metrics.Handler)
	r.Use(appmw.Logger(logger))
	// CORS policy — only explicitly allowed origins can make cross-origin requests.
	r.Use(appmw.CORS(corsOrigins))
	// Per-IP rate limiting.
	r.Use(appmw.RateLimit(rateLimitRPS, rateLimitBurst))
	// Security headers (nosniff, DENY framing, no-store cache).
	r.Use(appmw.SecurityHeaders)
	r.Use(chimw.Compress(5))

	// Health check endpoints — intentionally outside auth middleware
	// so that Kubernetes probes and load balancers can reach them.
	r.Get("/healthz", healthHandler.Liveness)
	r.Get("/readyz", healthHandler.Readiness)

	// Prometheus metrics endpoint — outside auth so Prometheus can scrape.
	r.Handle("/metrics", promhttp.Handler())

	// API v1 routes.
	r.Route("/api/v1", func(r chi.Router) {
		// API key authentication on all API routes.
		// When apiKey is empty, the middleware is a no-op (dev mode).
		// OWASP: logger is passed to enable auth failure logging for brute-force detection.
		r.Use(appmw.APIKeyAuth(apiKey, logger))

		// Tag routes.
		r.Route("/tags", func(r chi.Router) {
			r.Post("/", tagHandler.Create)
			r.Get("/", tagHandler.List)
			r.Patch("/{id}", tagHandler.Update)
			r.Delete("/{id}", tagHandler.Delete)
		})

		// Media routes.
		r.Route("/media", func(r chi.Router) {
			r.With(appmw.Idempotency(idempotencyStore)).Post("/", mediaHandler.Create)
			r.Get("/", mediaHandler.List)
			r.Get("/{id}", mediaHandler.GetByID)
			r.Delete("/{id}", mediaHandler.Delete)

			// Sub-resource for editing the tag list of an existing media
			// item without re-uploading the file.
			r.Post("/{id}/tags", mediaHandler.AttachTags)
			r.Delete("/{id}/tags/{tag_id}", mediaHandler.DetachTag)
		})
	})

	// Static file server for uploaded media.
	// Use noDirFS to prevent directory listing.
	fileServer := http.FileServer(noDirFS{http.Dir(uploadDir)})
	// Wrap with safeFileHeaders to force Content-Disposition: attachment,
	// preventing browsers from executing uploaded content inline (e.g. SVG with JS).
	r.Handle("/uploads/*", safeFileHeaders(http.StripPrefix("/uploads/", fileServer)))

	return r
}

// noDirFS wraps an http.FileSystem to prevent directory listing.
// Without this, http.FileServer exposes directory contents,
// allowing attackers to browse the hash-sharded upload tree and
// enumerate all files without knowing media IDs.
type noDirFS struct {
	fs http.FileSystem
}

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if stat.IsDir() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}

// safeFileHeaders wraps a handler to set security headers on file responses.
//
// Forces Content-Disposition: attachment so browsers download files instead
// of rendering them inline. This prevents stored XSS attacks where a
// malicious SVG/HTML file could execute JavaScript in the API's origin.
func safeFileHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
