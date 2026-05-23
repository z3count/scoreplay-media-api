// Package config provides application configuration loaded from environment
// variables. Every field has a sensible default for local development, but can
// be overridden in production by setting the corresponding env var.
//
// Design note: we deliberately avoid third-party config libraries (viper, envconfig)
// to keep the dependency tree minimal. os.Getenv + helper functions are sufficient
// for the scope of this application.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration values needed to run the application.
// Fields are grouped by concern: server, database, storage, and upload limits.
type Config struct {
	// Server settings
	ListenAddr        string        // Address to bind the HTTP server (e.g. ":8080")
	ReadHeaderTimeout time.Duration // Max time to read request headers (Slowloris protection)
	ReadTimeout       time.Duration // Max time to read entire request (including body)
	WriteTimeout      time.Duration // Max time to write response
	IdleTimeout       time.Duration // Max time for idle keep-alive connections
	ShutdownTimeout   time.Duration // Max time to wait for in-flight requests during shutdown
	RequestTimeout    time.Duration // Max time for a single request (context deadline)

	// Database settings
	DatabaseURL     string        // PostgreSQL connection string
	DBMaxOpenConns  int           // Max open connections to the database
	DBMaxIdleConns  int           // Max idle connections in the pool
	DBConnMaxLife   time.Duration // Max lifetime of a connection before it's recycled
	DBConnMaxIdle   time.Duration // Max time a connection can sit idle before being closed

	// Storage settings
	UploadDir string // Base directory for local file storage

	// Upload limits
	MaxUploadSize int64 // Maximum file upload size in bytes

	// Server base URL (used to construct file URLs in responses)
	BaseURL string

	// API key for authenticating requests.
	APIKey string

	// Rate limiting settings (per-IP token bucket).
	RateLimitRPS   float64 // Sustained requests per second per IP
	RateLimitBurst int     // Maximum burst size (initial token count)

	// Comma-separated list of allowed CORS origins.
	CORSOrigins []string

	// TLS certificate and key file paths.
	// When both are set, the server uses HTTPS. When empty, plain HTTP.
	// In production, prefer a reverse proxy (nginx/Caddy) for TLS termination.
	TLSCertFile string
	TLSKeyFile  string

	// Storage backend: "local" (default) or "s3".
	StorageBackend string

	// Background-job settings.
	// Backend selects the JobQueue implementation: "postgres" (default,
	// in-process worker polls with SKIP LOCKED) or "sqs" (Enqueue-only;
	// Lambda or an external consumer handles execution — the in-process
	// worker is not started in this mode).
	JobQueueBackend      string
	JobWorkerEnabled     bool          // Start the in-process worker (only meaningful for postgres backend)
	JobWorkerConcurrency int           // Number of polling goroutines
	JobPollInterval      time.Duration // How often each worker polls the queue
	JobRetentionDays     int           // Retain completed/failed jobs for N days
	JobCleanupInterval   time.Duration // How often the cleanup loop runs
	JobStatsInterval     time.Duration // How often queue-depth gauges are sampled

	// SQS configuration (used when JobQueueBackend = "sqs").
	SQSQueueURL string
	SQSRegion   string

	// S3 configuration (used when StorageBackend = "s3").
	S3Bucket    string // Bucket name
	S3Region    string // AWS region (e.g. "eu-west-1")
	S3Endpoint  string // Custom endpoint for S3-compatible services (MinIO, DO Spaces)
	S3AccessKey string // Access key (optional if using IAM roles)
	S3SecretKey string // Secret key (optional if using IAM roles)
	S3Prefix    string // Key prefix (e.g. "media/")
	S3CDNURL    string // CDN base URL for public file URLs
}

// Load reads configuration from environment variables, applying defaults for
// any value not explicitly set.
//
// This function never fails: all values have sensible defaults for local
// development. In production, the DATABASE_URL should always be set explicitly.
//
// Environment variables:
//   - LISTEN_ADDR:       Server bind address (default: ":8080")
//   - DATABASE_URL:      PostgreSQL DSN (default: local dev connection)
//   - UPLOAD_DIR:        File storage directory (default: "./uploads")
//   - MAX_UPLOAD_SIZE:   Max upload in bytes (default: 104857600 = 100MB)
//   - BASE_URL:          Public base URL (default: "http://localhost:8080")
//   - SHUTDOWN_TIMEOUT:  Graceful shutdown timeout in seconds (default: 30)
func Load() *Config {
	return &Config{
		ListenAddr:        getEnv("LISTEN_ADDR", ":8080"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ShutdownTimeout:   time.Duration(getEnvInt("SHUTDOWN_TIMEOUT", 30)) * time.Second,
		RequestTimeout:    time.Duration(getEnvInt("REQUEST_TIMEOUT", 30)) * time.Second,

		// In production, DATABASE_URL MUST be set via env var with
		// sslmode=require or sslmode=verify-full. The dev default uses
		// sslmode=disable for local convenience only.
		DatabaseURL:    getEnv("DATABASE_URL", "postgres://scoreplay:scoreplay@localhost:5432/media_api?sslmode=disable"),
		DBMaxOpenConns: getEnvInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns: getEnvInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLife:  time.Duration(getEnvInt("DB_CONN_MAX_LIFETIME", 300)) * time.Second,
		DBConnMaxIdle:  time.Duration(getEnvInt("DB_CONN_MAX_IDLE_TIME", 60)) * time.Second,

		UploadDir: getEnv("UPLOAD_DIR", "./uploads"),

		MaxUploadSize: int64(getEnvInt("MAX_UPLOAD_SIZE", 100*1024*1024)), // 100 MB

		BaseURL: getEnv("BASE_URL", "http://localhost:8080"),

		APIKey: getEnv("API_KEY", ""),

		RateLimitRPS:   float64(getEnvInt("RATE_LIMIT_RPS", 10)),
		RateLimitBurst: getEnvInt("RATE_LIMIT_BURST", 30),

		CORSOrigins: parseCORSOrigins(getEnv("CORS_ORIGINS", "")),

		// TLS configuration. Set both to enable HTTPS.
		TLSCertFile: getEnv("TLS_CERT_FILE", ""),
		TLSKeyFile:  getEnv("TLS_KEY_FILE", ""),

		// Storage backend selection.
		StorageBackend: getEnv("STORAGE_BACKEND", "local"),

		// S3 configuration (only used when STORAGE_BACKEND=s3).
		S3Bucket:    getEnv("S3_BUCKET", ""),
		S3Region:    getEnv("S3_REGION", ""),
		S3Endpoint:  getEnv("S3_ENDPOINT", ""),
		S3AccessKey: getEnv("S3_ACCESS_KEY", ""),
		S3SecretKey: getEnv("S3_SECRET_KEY", ""),
		S3Prefix:    getEnv("S3_PREFIX", ""),
		S3CDNURL:    getEnv("S3_CDN_URL", ""),

		// Background-job settings. Defaults: postgres backend, worker on,
		// 4 polling goroutines @ 2s, 7-day retention, 24h cleanup, 15s stats.
		JobQueueBackend:      getEnv("JOB_QUEUE_BACKEND", "postgres"),
		JobWorkerEnabled:     getEnvBool("JOB_WORKER_ENABLED", true),
		JobWorkerConcurrency: getEnvInt("JOB_WORKER_CONCURRENCY", 4),
		JobPollInterval:      time.Duration(getEnvInt("JOB_POLL_INTERVAL", 2)) * time.Second,
		JobRetentionDays:     getEnvInt("JOB_RETENTION_DAYS", 7),
		JobCleanupInterval:   time.Duration(getEnvInt("JOB_CLEANUP_INTERVAL", 24)) * time.Hour,
		JobStatsInterval:     time.Duration(getEnvInt("JOB_STATS_INTERVAL", 15)) * time.Second,

		// SQS configuration (only used when JOB_QUEUE_BACKEND=sqs).
		SQSQueueURL: getEnv("SQS_QUEUE_URL", ""),
		SQSRegion:   getEnv("SQS_REGION", ""),
	}
}

// String returns a redacted string representation of the configuration,
// suitable for logging at startup. The DATABASE_URL is masked to avoid
// leaking credentials in log output.
func (c *Config) String() string {
	return fmt.Sprintf(
		"ListenAddr=%s DatabaseURL=<redacted> UploadDir=%s MaxUploadSize=%d BaseURL=%s",
		c.ListenAddr, c.UploadDir, c.MaxUploadSize, c.BaseURL,
	)
}

// getEnv returns the value of the environment variable named by key,
// or fallback if the variable is not set or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvInt returns the integer value of the environment variable named by key,
// or fallback if the variable is not set, empty, or not a valid integer.
// Parsing errors are silently ignored and the fallback is returned.
func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// getEnvBool returns the boolean value of the environment variable named by
// key. Accepts the values strconv.ParseBool recognises (1/0, t/f, true/false,
// yes/no — case-insensitive); falls back on parse error or missing var.
func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "t", "true", "y", "yes", "on":
			return true
		case "0", "f", "false", "n", "no", "off":
			return false
		}
	}
	return fallback
}

// parseCORSOrigins splits a comma-separated string of origins into a slice.
func parseCORSOrigins(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}
