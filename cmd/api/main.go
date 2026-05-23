// Package main is the entry point for the ScorePlay Media API.
//
// It is responsible for:
//  1. Loading configuration from environment variables.
//  2. Connecting to the PostgreSQL database and running migrations.
//  3. Initializing all dependencies (repositories, storage, services, handlers).
//  4. Starting the HTTP server with production-grade timeouts.
//  5. Graceful shutdown: listening for OS signals and draining in-flight requests.
//
// This follows the "composition root" pattern: all dependency wiring happens
// here in main(), and dependencies flow downward through constructor injection.
// No global state, no init() functions, no singletons.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/scoreplay/media-api/config"
	"github.com/scoreplay/media-api/internal/adapter/http/handler"
	pgadapter "github.com/scoreplay/media-api/internal/adapter/postgres"
	appserver "github.com/scoreplay/media-api/internal/adapter/server"
	sqsadapter "github.com/scoreplay/media-api/internal/adapter/sqs"
	"github.com/scoreplay/media-api/internal/adapter/storage/local"
	s3storage "github.com/scoreplay/media-api/internal/adapter/storage/s3"
	"github.com/scoreplay/media-api/internal/port"
	"github.com/scoreplay/media-api/internal/service"
)

func main() {
	// Initialize structured logger.
	// JSON by default for production log aggregation (ELK, Datadog, CloudWatch).
	// Set LOG_FORMAT=text for human-readable output during local development.
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)

	if err := run(logger); err != nil {
		logger.Error("application failed", "error", err)
		os.Exit(1)
	}
}

// run contains the actual application logic, separated from main() for testability.
// It returns an error instead of calling os.Exit(), making it possible to test
// the startup/shutdown sequence.
//
// Lifecycle:
//  1. Load config → 2. Connect DB → 3. Run migrations → 4. Init storage →
//  5. Wire dependencies → 6. Start HTTP server → 7. Wait for signal →
//  8. Graceful shutdown (drain requests, close DB)
func run(logger *slog.Logger) error {
	// Step 1: Load configuration.
	cfg := config.Load()
	logger.Info("configuration loaded", "config", cfg.String())

	// Step 2: Connect to PostgreSQL.
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// Verify the connection is actually working (sql.Open only validates the DSN).
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("database connected")

	// OWASP Communication Security: warn if DB connection is not encrypted.
	// In production, sslmode should be "verify-full" or at least "require".
	if strings.Contains(cfg.DatabaseURL, "sslmode=disable") {
		logger.Warn("SECURITY: database connection is NOT encrypted (sslmode=disable)")
	}

	// OWASP Authentication: warn if API key is not configured.
	if cfg.APIKey == "" {
		logger.Warn("SECURITY: API_KEY is not set — authentication is disabled")
	}

	// Configure connection pool for production use.
	// See: https://www.alexedwards.net/blog/configuring-sqldb
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLife)
	db.SetConnMaxIdleTime(cfg.DBConnMaxIdle)

	// Expose database/sql pool stats as Prometheus metrics. This surfaces
	// the #1 saturation signal for a Postgres-backed API: pool exhaustion
	// (WaitCount, WaitDuration, InUse). Without this, a backed-up pool is
	// invisible until requests start timing out.
	if err := prometheus.Register(collectors.NewDBStatsCollector(db, "media_api")); err != nil {
		// A duplicate registration is non-fatal (e.g., in tests that re-run
		// run()); other errors should surface as warnings, not crash startup.
		var dup prometheus.AlreadyRegisteredError
		if !errors.As(err, &dup) {
			logger.Warn("db stats collector registration failed", "error", err)
		}
	}

	// Step 3: Run database migrations.
	if err := runMigrations(db, logger); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Step 4: Initialize file storage backend.
	var storage port.FileStorage
	switch cfg.StorageBackend {
	case "s3":
		s3Store, err := s3storage.NewStorage(context.Background(), s3storage.Config{
			Bucket:    cfg.S3Bucket,
			Region:    cfg.S3Region,
			Endpoint:  cfg.S3Endpoint,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
			Prefix:    cfg.S3Prefix,
			CDNURL:    cfg.S3CDNURL,
		})
		if err != nil {
			return fmt.Errorf("init S3 storage: %w", err)
		}
		storage = s3Store
		logger.Info("file storage initialized", "backend", "s3", "bucket", cfg.S3Bucket, "region", cfg.S3Region)

		// OWASP Credential Management: warn if static AWS keys are configured
		// in production. The intended path is the AWS SDK default credential
		// chain (IRSA / task role / instance profile), which delivers short-
		// lived STS credentials that rotate automatically. Static keys are
		// fine for local dev against MinIO but should never reach production.
		if cfg.S3AccessKey != "" {
			logger.Warn("SECURITY: static AWS credentials are set (S3_ACCESS_KEY) — prefer IAM roles in production (IRSA / task role / instance profile)")
		}
	default:
		localStorage, err := local.NewStorage(cfg.UploadDir)
		if err != nil {
			return fmt.Errorf("init local storage: %w", err)
		}
		storage = localStorage
		logger.Info("file storage initialized", "backend", "local", "dir", cfg.UploadDir)
	}

	// Step 5: Wire dependencies (dependency injection via constructors).
	tagRepo := pgadapter.NewTagRepo(db)
	mediaRepo := pgadapter.NewMediaRepo(db)
	idempotencyStore := pgadapter.NewIdempotencyStore(db)

	// Single root context for all long-lived background goroutines (queue
	// stats sampler, queue cleanup, in-process worker). Cancelled during
	// graceful shutdown AFTER the HTTP server has drained, so in-flight
	// HTTP handlers can still enqueue jobs while shutting down.
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	// Start periodic cleanup of expired idempotency keys (every hour).
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				if err := idempotencyStore.Cleanup(context.Background()); err != nil {
					logger.Error("idempotency key cleanup failed", "error", err)
				}
			}
		}
	}()

	// Step 5a: Background-job plumbing.
	// JobEnqueuer (producer side) is always wired so services can fire-and-
	// forget. The in-process worker (consumer side) only starts for the
	// postgres backend — in SQS mode an external consumer (Lambda) executes
	// handlers, and starting the worker would be a no-op anyway since
	// sqs.JobQueue.Dequeue returns nil.
	jobQueue, err := newJobQueue(bgCtx, cfg, db, logger)
	if err != nil {
		return fmt.Errorf("init job queue: %w", err)
	}

	// Handler registry: maps job type → handler. To add a new background
	// task (e.g., thumbnail, transcode), implement port.JobHandler and
	// register it here. The noop handler ships as a smoke test so the full
	// enqueue → dispatch → complete pipeline can be verified end-to-end
	// without writing real handler code.
	jobHandlers := map[string]port.JobHandler{
		service.JobTypeNoop: service.NewNoopJobHandler(),
	}

	var workerWG sync.WaitGroup
	if cfg.JobQueueBackend == "postgres" && cfg.JobWorkerEnabled {
		worker := service.NewWorker(
			jobQueue,
			jobHandlers,
			logger,
			cfg.JobPollInterval,
			cfg.JobWorkerConcurrency,
		)
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			worker.Run(bgCtx)
		}()
	} else {
		logger.Info("in-process job worker not started",
			"backend", cfg.JobQueueBackend,
			"worker_enabled", cfg.JobWorkerEnabled,
		)
	}

	tagSvc := service.NewTagService(tagRepo)
	mediaSvc := service.NewMediaService(mediaRepo, tagRepo, storage, cfg.BaseURL, logger)
	// jobQueue (port.JobEnqueuer) is intentionally not yet wired into
	// tagSvc/mediaSvc — services will accept port.JobEnqueuer in their
	// constructors once a feature (thumbnail, transcode, …) needs to
	// enqueue work. See DESIGN.md → "Background jobs" for the plug-in
	// pattern.

	tagHandler := handler.NewTagHandler(tagSvc, logger)
	mediaHandler := handler.NewMediaHandler(mediaSvc, logger, cfg.MaxUploadSize)
	healthHandler := handler.NewHealthHandler(db)

	router := appserver.NewRouter(healthHandler, tagHandler, mediaHandler, cfg.UploadDir, cfg.APIKey, cfg.CORSOrigins, cfg.RequestTimeout, cfg.RateLimitRPS, cfg.RateLimitBurst, idempotencyStore, logger)

	// Step 6: Configure and start HTTP server.
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Start server in a goroutine so we can listen for shutdown signals.
	serverErr := make(chan error, 1)
	go func() {
		// Use TLS when certificate and key are configured.
		// In production, prefer a reverse proxy (nginx/Caddy) with TLS termination.
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			logger.Info("starting HTTPS server", "addr", cfg.ListenAddr, "cert", cfg.TLSCertFile)
			serverErr <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			logger.Info("starting HTTP server", "addr", cfg.ListenAddr)
			serverErr <- srv.ListenAndServe()
		}
	}()

	// Step 7: Wait for interrupt signal or server error.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-serverErr:
		if err != http.ErrServerClosed {
			return fmt.Errorf("server error: %w", err)
		}
	}

	// Step 8: Graceful shutdown.
	// Ordering matters:
	//  1. srv.Shutdown stops accepting new HTTP requests and drains in-flight
	//     ones (which may still enqueue jobs during this window).
	//  2. cancelBg signals workers, stats sampler, and cleanup loops to stop.
	//  3. workerWG.Wait blocks until the worker pool finishes its current
	//     batch — handlers see ctx.Done() and should return promptly.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	logger.Info("shutting down server", "timeout", cfg.ShutdownTimeout.String())
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}

	cancelBg()
	waitWithTimeout(&workerWG, cfg.ShutdownTimeout, logger)

	logger.Info("server stopped gracefully")
	return nil
}

// newJobQueue builds the JobQueue implementation selected by config and, for
// the postgres backend, also starts the stats-sampling and cleanup loops.
// Both loops are bound to bgCtx and stop when it is cancelled.
func newJobQueue(bgCtx context.Context, cfg *config.Config, db *sql.DB, logger *slog.Logger) (port.JobQueue, error) {
	switch cfg.JobQueueBackend {
	case "sqs":
		if cfg.SQSQueueURL == "" {
			return nil, fmt.Errorf("JOB_QUEUE_BACKEND=sqs requires SQS_QUEUE_URL")
		}
		logger.Info("job queue initialized", "backend", "sqs", "queue_url", cfg.SQSQueueURL, "region", cfg.SQSRegion)
		return sqsadapter.NewJobQueue(sqsadapter.Config{
			QueueURL: cfg.SQSQueueURL,
			Region:   cfg.SQSRegion,
		}), nil

	case "postgres", "":
		q := pgadapter.NewJobQueue(db)
		// Postgres-only sidecar loops:
		//   - StartStatsLoop populates the saturation gauges every JobStatsInterval.
		//   - StartCleanupLoop deletes old completed/failed jobs daily.
		go q.StartStatsLoop(bgCtx, cfg.JobStatsInterval, logger)
		go q.StartCleanupLoop(bgCtx, cfg.JobCleanupInterval, cfg.JobRetentionDays)
		logger.Info("job queue initialized",
			"backend", "postgres",
			"stats_interval", cfg.JobStatsInterval.String(),
			"cleanup_interval", cfg.JobCleanupInterval.String(),
			"retention_days", cfg.JobRetentionDays,
		)
		return q, nil

	default:
		return nil, fmt.Errorf("unknown JOB_QUEUE_BACKEND %q (want postgres|sqs)", cfg.JobQueueBackend)
	}
}

// waitWithTimeout blocks until wg is done or the timeout elapses, logging a
// warning if the wait runs out. Used during graceful shutdown so a stuck
// worker handler can't pin the process forever.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Warn("background workers did not stop within shutdown timeout", "timeout", timeout.String())
	}
}

// runMigrations applies pending database migrations using golang-migrate.
//
// Migrations are embedded in the binary via go:embed, so they are always
// in sync with the application version. This is safer than reading from
// the filesystem, where migration files could be missing or out of date.
//
// Behavior:
//   - Applies all pending "up" migrations in order.
//   - If no new migrations are pending, logs and continues (no error).
//   - If a migration fails, returns the error (the application will not start
//     with an inconsistent schema).
func runMigrations(db *sql.DB, logger *slog.Logger) error {
	sourceDriver, err := iofs.New(pgadapter.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}

	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		return fmt.Errorf("create migration db driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", dbDriver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("apply migrations: %w", err)
	}

	logger.Info("database migrations applied")
	return nil
}
