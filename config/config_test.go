package config

import (
	"os"
	"testing"
	"time"
)

// TestLoad_TLSConfig verifies that TLS configuration is loaded from env vars.
func TestLoad_TLSConfig(t *testing.T) {
	// Default: no TLS.
	cfg := Load()
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		t.Fatal("expected empty TLS config by default")
	}

	// Set TLS env vars.
	os.Setenv("TLS_CERT_FILE", "/path/to/cert.pem")
	os.Setenv("TLS_KEY_FILE", "/path/to/key.pem")
	defer os.Unsetenv("TLS_CERT_FILE")
	defer os.Unsetenv("TLS_KEY_FILE")

	cfg = Load()
	if cfg.TLSCertFile != "/path/to/cert.pem" {
		t.Fatalf("expected cert path, got %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/path/to/key.pem" {
		t.Fatalf("expected key path, got %q", cfg.TLSKeyFile)
	}
}

// TestLoad_DBPoolDefaults verifies sensible defaults for DB connection pool.
func TestLoad_DBPoolDefaults(t *testing.T) {
	// Clear any env vars that might interfere.
	for _, key := range []string{"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME", "DB_CONN_MAX_IDLE_TIME"} {
		os.Unsetenv(key)
	}

	cfg := Load()

	if cfg.DBMaxOpenConns != 25 {
		t.Errorf("expected DBMaxOpenConns=25, got %d", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 5 {
		t.Errorf("expected DBMaxIdleConns=5, got %d", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLife != 300*time.Second {
		t.Errorf("expected DBConnMaxLife=5m, got %v", cfg.DBConnMaxLife)
	}
	if cfg.DBConnMaxIdle != 60*time.Second {
		t.Errorf("expected DBConnMaxIdle=1m, got %v", cfg.DBConnMaxIdle)
	}
}

// TestLoad_DBPoolEnvOverrides verifies that DB pool settings can be
// overridden via environment variables.
func TestLoad_DBPoolEnvOverrides(t *testing.T) {
	os.Setenv("DB_MAX_OPEN_CONNS", "50")
	os.Setenv("DB_MAX_IDLE_CONNS", "10")
	os.Setenv("DB_CONN_MAX_LIFETIME", "600")
	os.Setenv("DB_CONN_MAX_IDLE_TIME", "120")
	defer func() {
		os.Unsetenv("DB_MAX_OPEN_CONNS")
		os.Unsetenv("DB_MAX_IDLE_CONNS")
		os.Unsetenv("DB_CONN_MAX_LIFETIME")
		os.Unsetenv("DB_CONN_MAX_IDLE_TIME")
	}()

	cfg := Load()

	if cfg.DBMaxOpenConns != 50 {
		t.Errorf("expected DBMaxOpenConns=50, got %d", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 10 {
		t.Errorf("expected DBMaxIdleConns=10, got %d", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLife != 600*time.Second {
		t.Errorf("expected DBConnMaxLife=10m, got %v", cfg.DBConnMaxLife)
	}
	if cfg.DBConnMaxIdle != 120*time.Second {
		t.Errorf("expected DBConnMaxIdle=2m, got %v", cfg.DBConnMaxIdle)
	}
}

// TestLoad_DBPoolInvalidEnv verifies that invalid env values fall back to
// defaults (no crash, no error).
func TestLoad_DBPoolInvalidEnv(t *testing.T) {
	os.Setenv("DB_MAX_OPEN_CONNS", "not-a-number")
	os.Setenv("DB_MAX_IDLE_CONNS", "")
	defer func() {
		os.Unsetenv("DB_MAX_OPEN_CONNS")
		os.Unsetenv("DB_MAX_IDLE_CONNS")
	}()

	cfg := Load()

	if cfg.DBMaxOpenConns != 25 {
		t.Errorf("expected fallback to 25, got %d", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 5 {
		t.Errorf("expected fallback to 5, got %d", cfg.DBMaxIdleConns)
	}
}

// TestLoad_JobQueueDefaults pins the defaults for the background-job
// configuration so accidental changes show up in code review rather than
// in production.
func TestLoad_JobQueueDefaults(t *testing.T) {
	for _, key := range []string{
		"JOB_QUEUE_BACKEND", "JOB_WORKER_ENABLED", "JOB_WORKER_CONCURRENCY",
		"JOB_POLL_INTERVAL", "JOB_RETENTION_DAYS", "JOB_CLEANUP_INTERVAL",
		"JOB_STATS_INTERVAL", "SQS_QUEUE_URL", "SQS_REGION",
	} {
		os.Unsetenv(key)
	}

	cfg := Load()

	if cfg.JobQueueBackend != "postgres" {
		t.Errorf("backend default: want postgres, got %q", cfg.JobQueueBackend)
	}
	if !cfg.JobWorkerEnabled {
		t.Error("worker should be enabled by default")
	}
	if cfg.JobWorkerConcurrency != 4 {
		t.Errorf("concurrency default: want 4, got %d", cfg.JobWorkerConcurrency)
	}
	if cfg.JobPollInterval != 2*time.Second {
		t.Errorf("poll interval default: want 2s, got %v", cfg.JobPollInterval)
	}
	if cfg.JobRetentionDays != 7 {
		t.Errorf("retention default: want 7, got %d", cfg.JobRetentionDays)
	}
	if cfg.JobCleanupInterval != 24*time.Hour {
		t.Errorf("cleanup interval default: want 24h, got %v", cfg.JobCleanupInterval)
	}
	if cfg.JobStatsInterval != 15*time.Second {
		t.Errorf("stats interval default: want 15s, got %v", cfg.JobStatsInterval)
	}
}

// TestLoad_JobQueueOverrides verifies env-var overrides take effect across
// the full set of job-queue fields.
func TestLoad_JobQueueOverrides(t *testing.T) {
	os.Setenv("JOB_QUEUE_BACKEND", "sqs")
	os.Setenv("JOB_WORKER_ENABLED", "false")
	os.Setenv("JOB_WORKER_CONCURRENCY", "16")
	os.Setenv("JOB_POLL_INTERVAL", "5")
	os.Setenv("JOB_RETENTION_DAYS", "30")
	os.Setenv("JOB_CLEANUP_INTERVAL", "6")
	os.Setenv("JOB_STATS_INTERVAL", "60")
	os.Setenv("SQS_QUEUE_URL", "https://sqs.eu-west-1.amazonaws.com/123/q")
	os.Setenv("SQS_REGION", "eu-west-1")
	defer func() {
		for _, key := range []string{
			"JOB_QUEUE_BACKEND", "JOB_WORKER_ENABLED", "JOB_WORKER_CONCURRENCY",
			"JOB_POLL_INTERVAL", "JOB_RETENTION_DAYS", "JOB_CLEANUP_INTERVAL",
			"JOB_STATS_INTERVAL", "SQS_QUEUE_URL", "SQS_REGION",
		} {
			os.Unsetenv(key)
		}
	}()

	cfg := Load()

	if cfg.JobQueueBackend != "sqs" {
		t.Errorf("backend: want sqs, got %q", cfg.JobQueueBackend)
	}
	if cfg.JobWorkerEnabled {
		t.Error("worker should be disabled")
	}
	if cfg.JobWorkerConcurrency != 16 {
		t.Errorf("concurrency: want 16, got %d", cfg.JobWorkerConcurrency)
	}
	if cfg.JobPollInterval != 5*time.Second {
		t.Errorf("poll interval: want 5s, got %v", cfg.JobPollInterval)
	}
	if cfg.JobRetentionDays != 30 {
		t.Errorf("retention: want 30, got %d", cfg.JobRetentionDays)
	}
	if cfg.JobCleanupInterval != 6*time.Hour {
		t.Errorf("cleanup interval: want 6h, got %v", cfg.JobCleanupInterval)
	}
	if cfg.JobStatsInterval != 60*time.Second {
		t.Errorf("stats interval: want 60s, got %v", cfg.JobStatsInterval)
	}
	if cfg.SQSQueueURL != "https://sqs.eu-west-1.amazonaws.com/123/q" {
		t.Errorf("sqs url: got %q", cfg.SQSQueueURL)
	}
	if cfg.SQSRegion != "eu-west-1" {
		t.Errorf("sqs region: got %q", cfg.SQSRegion)
	}
}

// TestGetEnvBool covers every parse path: each accepted spelling, the fallback
// for unrecognised values, and the fallback for unset values.
func TestGetEnvBool(t *testing.T) {
	const key = "TEST_JOB_BOOL"
	cases := []struct {
		value    string // empty means "unset"
		want     bool
		fallback bool
	}{
		// Truthy spellings (all return true regardless of fallback).
		{"1", true, false}, {"true", true, false}, {"TRUE", true, false},
		{"t", true, false}, {"yes", true, false}, {"on", true, false},

		// Falsy spellings.
		{"0", false, true}, {"false", false, true}, {"FALSE", false, true},
		{"f", false, true}, {"no", false, true}, {"off", false, true},

		// Unrecognised → fallback.
		{"maybe", true, true}, {"maybe", false, false},

		// Unset → fallback.
		{"", true, true}, {"", false, false},
	}

	for _, c := range cases {
		t.Run(c.value+"_fallback_"+boolStr(c.fallback), func(t *testing.T) {
			if c.value == "" {
				os.Unsetenv(key)
			} else {
				os.Setenv(key, c.value)
				defer os.Unsetenv(key)
			}
			got := getEnvBool(key, c.fallback)
			if got != c.want {
				t.Fatalf("getEnvBool(%q, %v) = %v, want %v", c.value, c.fallback, got, c.want)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
