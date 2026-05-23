// Package e2e contains end-to-end tests that spin up a real PostgreSQL container
// via testcontainers, start the full HTTP server, and exercise the API through
// actual HTTP requests.
//
// These tests validate the entire request lifecycle: HTTP parsing → service logic
// → database queries → file storage → HTTP response. They catch integration bugs
// that unit tests with mocks cannot detect (e.g., SQL syntax errors, migration
// issues, middleware ordering problems).
//
// Requirements:
//   - Docker must be running (testcontainers uses it to start PostgreSQL).
//   - Tests are skipped in short mode (-short flag) to keep CI fast.
package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/scoreplay/media-api/internal/adapter/http/handler"
	pgadapter "github.com/scoreplay/media-api/internal/adapter/postgres"
	appserver "github.com/scoreplay/media-api/internal/adapter/server"
	"github.com/scoreplay/media-api/internal/adapter/storage/local"
	"github.com/scoreplay/media-api/internal/service"
)

// testAPIKey is the API key used by e2e tests.
const testAPIKey = "test-api-key-e2e"

// testEnv holds all shared resources for e2e tests: the database connection,
// the HTTP server, and the base URL to send requests to.
type testEnv struct {
	db        *sql.DB
	server    *http.Server
	baseURL   string
	uploadDir string
	client    *http.Client // pre-configured client with API key header
}

// authTransport is an http.RoundTripper that injects the X-API-Key header
// into every outgoing request. This avoids repeating auth boilerplate in
// every test helper.
type authTransport struct {
	apiKey string
	base   http.RoundTripper
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-API-Key", t.apiKey)
	return t.base.RoundTrip(req)
}

// setupTestEnv creates a full test environment:
//  1. Starts a PostgreSQL container via testcontainers.
//  2. Runs database migrations.
//  3. Initializes all application layers (repos, services, handlers).
//  4. Starts an HTTP server on a random port.
//
// The caller must call the returned cleanup function when done.
// Tests using this function should call t.Skip in short mode.
func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	ctx := context.Background()

	// Step 1: Start PostgreSQL container.
	pgContainer, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("media_api_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { pgContainer.Terminate(context.Background()) })

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}

	// Step 2: Connect and run migrations.
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	runMigrations(t, db)

	// Step 3: Initialize storage.
	uploadDir := t.TempDir()
	storage, err := local.NewStorage(uploadDir)
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}

	// Step 4: Wire dependencies.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	tagRepo := pgadapter.NewTagRepo(db)
	mediaRepo := pgadapter.NewMediaRepo(db)

	tagSvc := service.NewTagService(tagRepo)
	mediaSvc := service.NewMediaService(mediaRepo, tagRepo, storage, "http://localhost:0", logger)

	tagHandler := handler.NewTagHandler(tagSvc, logger)
	mediaHandler := handler.NewMediaHandler(mediaSvc, logger, 10*1024*1024) // 10MB for tests
	healthHandler := handler.NewHealthHandler(db)
	idempotencyStore := pgadapter.NewIdempotencyStore(db)

	// Auth: provision the legacy tenant and register testAPIKey against it
	// (same bootstrap path main.go uses).
	authVerifier := pgadapter.NewAuthVerifier(db, 0) // cacheTTL=0 for tests
	if err := authVerifier.EnsureLegacyTenant(ctx, testAPIKey); err != nil {
		t.Fatalf("ensure legacy tenant: %v", err)
	}

	// Configure router with the verifier; no dev bypass (devTenantID=nil)
	// so the tests exercise real auth.
	router := appserver.NewRouter(healthHandler, tagHandler, mediaHandler, uploadDir, authVerifier, nil, nil, 30*time.Second, 100, 200, idempotencyStore, logger)

	// Step 5: Start HTTP server on a random port.
	srv := &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: router,
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	baseURL := fmt.Sprintf("http://%s", ln.Addr().String())

	// Create an HTTP client that auto-injects the API key header.
	authClient := &http.Client{
		Transport: &authTransport{
			apiKey: testAPIKey,
			base:   http.DefaultTransport,
		},
	}

	return &testEnv{
		db:        db,
		server:    srv,
		baseURL:   baseURL,
		uploadDir: uploadDir,
		client:    authClient,
	}
}

// runMigrations applies all database migrations using the embedded SQL files.
func runMigrations(t *testing.T, db *sql.DB) {
	t.Helper()

	sourceDriver, err := iofs.New(pgadapter.Migrations, "migrations")
	if err != nil {
		t.Fatalf("create migration source: %v", err)
	}

	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("create migration db driver: %v", err)
	}

	m, err := migrate.NewWithInstance("iofs", sourceDriver, "postgres", dbDriver)
	if err != nil {
		t.Fatalf("create migrator: %v", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("apply migrations: %v", err)
	}
}
