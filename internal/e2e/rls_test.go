package e2e

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	pgadapter "github.com/scoreplay/media-api/internal/adapter/postgres"
)

// TestE2E_RLSBackstop verifies that Postgres Row-Level Security enforces
// tenant isolation at the database level — i.e. even if a future query
// is written WITHOUT a `WHERE tenant_id = $X` filter, the rows are still
// scoped by the session's app.tenant_id setting.
//
// This is the load-bearing test for migration 006_rls. The application
// already filters every query; RLS is the safety net that makes the
// system robust to a missed WHERE clause.
//
// Scenarios:
//
//  1. With app.tenant_id = '<A>' set: an unfiltered SELECT * FROM tags
//     returns only tenant A's rows, even though tenant B has more.
//  2. With app.tenant_id unset: the same query returns zero rows
//     (default-deny — no implicit "see everything").
//  3. With app.system_mode = '1' set: the same query returns all rows
//     across tenants — the system bypass for maintenance ops.
//  4. INSERT with app.tenant_id = '<A>' and a row claiming tenant_id =
//     '<B>' is rejected by the policy's WITH CHECK clause.
func TestE2E_RLSBackstop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	pgEnv := setupTestEnv(t)

	// Postgres bypasses RLS for SUPERUSER and BYPASSRLS roles. The
	// testcontainer's default user is both. To actually exercise RLS, the
	// test queries run on a freshly-created role that is neither. Owner
	// privileges on the tables are granted explicitly so the role can
	// SELECT/INSERT/UPDATE/DELETE — RLS then filters those operations.
	//
	// In a production deployment the application would be configured to
	// connect with a dedicated non-superuser role from the start; this
	// helper just simulates that for the test.
	db := connectAsAppRole(t, pgEnv.db, pgEnv.dsn)

	verifier := pgadapter.NewAuthVerifier(pgEnv.db, 0)
	tenantA, err := verifier.CreateTenant(context.Background(), "rls-a")
	if err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	tenantB, err := verifier.CreateTenant(context.Background(), "rls-b")
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}

	seedTag(t, db, tenantA, "tag-a-only")
	seedTag(t, db, tenantB, "tag-b1")
	seedTag(t, db, tenantB, "tag-b2")

	// --- 1. With app.tenant_id = A, only A's rows are visible ---
	asTenant(t, db, tenantA, func(tx *sql.Tx) {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
			t.Fatalf("count as A: %v", err)
		}
		if n != 1 {
			t.Errorf("as tenant A, unfiltered tag count: want 1 (own row only), got %d", n)
		}

		var name string
		if err := tx.QueryRow(`SELECT name FROM tags`).Scan(&name); err != nil {
			t.Fatalf("select name as A: %v", err)
		}
		if name != "tag-a-only" {
			t.Errorf("as tenant A: want name 'tag-a-only', got %q", name)
		}
	})

	// --- 2. With no app.tenant_id, zero rows visible (default-deny) ---
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin neutral tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
		tx.Rollback()
		t.Fatalf("count neutral: %v", err)
	}
	tx.Rollback()
	if n != 0 {
		t.Errorf("no tenant set, unfiltered tag count: want 0 (default-deny), got %d", n)
	}

	// --- 3. With app.system_mode = '1', all rows visible (bypass) ---
	asSystem(t, db, func(tx *sql.Tx) {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
			t.Fatalf("count as system: %v", err)
		}
		if n != 3 { // 1 for A + 2 for B
			t.Errorf("system mode, unfiltered tag count: want 3, got %d", n)
		}
	})

	// --- 4. WITH CHECK refuses cross-tenant INSERT ---
	// Tenant A's session tries to insert a row claiming tenant_id = B.
	// Postgres rejects with "new row violates row-level security policy".
	asTenant(t, db, tenantA, func(tx *sql.Tx) {
		_, err := tx.Exec(
			`INSERT INTO tags (tenant_id, name) VALUES ($1, 'forbidden')`,
			tenantB,
		)
		if err == nil {
			t.Error("WITH CHECK: cross-tenant INSERT was accepted (RLS not enforced)")
			return
		}
		// pq error message is "new row violates row-level security policy for table".
		// We check substring rather than the exact wording to stay resilient
		// to Postgres version differences.
		if !containsRLS(err.Error()) {
			t.Errorf("WITH CHECK: expected RLS rejection, got: %v", err)
		}
	})
}

// connectAsAppRole creates a fresh non-superuser role on the existing
// (superuser) DB connection, grants it ownership-equivalent rights on
// the domain tables, then opens a new *sql.DB connected as that role.
// RLS policies apply to queries through the returned handle.
//
// The role + grants are torn down on test cleanup.
func connectAsAppRole(t *testing.T, adminDB *sql.DB, baseDSN string) *sql.DB {
	t.Helper()

	const role = "rls_test_app"
	const pw = "rls_test_app_pw"

	// CREATE the role (no superuser, no bypassrls). Grants are kept
	// minimal — exactly what the application needs in production.
	stmts := []string{
		`DROP OWNED BY ` + role + ` CASCADE`,                 // best-effort, ignored on first run
		`DROP ROLE IF EXISTS ` + role,
		`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + pw + `' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA public TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO ` + role,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO ` + role,
	}
	for _, s := range stmts {
		_, _ = adminDB.Exec(s) // best-effort; CREATE may say role exists, that's fine
	}
	// One required statement that we DO want to surface failures from.
	if _, err := adminDB.Exec(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role); err != nil {
		t.Fatalf("grant table privileges: %v", err)
	}

	// Build a DSN with the new credentials by string-replacing user:pass.
	// baseDSN looks like postgres://test:test@host:port/db?sslmode=disable.
	dsn := replaceUserPass(baseDSN, role, pw)
	appDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open app-role db: %v", err)
	}
	if err := appDB.Ping(); err != nil {
		t.Fatalf("ping app-role db: %v", err)
	}
	t.Cleanup(func() {
		appDB.Close()
		_, _ = adminDB.Exec(`REASSIGN OWNED BY ` + role + ` TO CURRENT_USER`)
		_, _ = adminDB.Exec(`DROP OWNED BY ` + role)
		_, _ = adminDB.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	return appDB
}

// replaceUserPass rewrites the userinfo portion of a postgres URL DSN.
// Input:  postgres://test:test@host:5432/db?...
// Output: postgres://<role>:<pw>@host:5432/db?...
// The function is intentionally tiny — full URL parsing isn't needed
// because testcontainers always emits this exact shape.
func replaceUserPass(dsn, user, pw string) string {
	// Find the "://" and the first '@' after it.
	const sep = "://"
	i := indexOf(dsn, sep)
	if i < 0 {
		return dsn
	}
	j := i + len(sep)
	at := indexOf(dsn[j:], "@")
	if at < 0 {
		return dsn
	}
	return dsn[:j] + user + ":" + pw + dsn[j+at:]
}

// asTenant runs fn inside a transaction with SET LOCAL app.tenant_id
// set to the given uuid. Used to simulate what postgres.withTenantTx
// does, but inline for direct RLS verification.
func asTenant(t *testing.T, db *sql.DB, tenantID uuid.UUID, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec("SET LOCAL app.tenant_id = '" + tenantID.String() + "'"); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	fn(tx)
}

// asSystem runs fn inside a transaction with SET LOCAL app.system_mode
// set to '1' — bypasses RLS the way postgres.withSystemTx does.
func asSystem(t *testing.T, db *sql.DB, fn func(tx *sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec("SET LOCAL app.system_mode = '1'"); err != nil {
		t.Fatalf("set system mode: %v", err)
	}
	fn(tx)
}

// seedTag inserts a tag directly into the database under the given
// tenant. Uses asTenant so the insert passes the WITH CHECK policy.
func seedTag(t *testing.T, db *sql.DB, tenantID uuid.UUID, name string) {
	t.Helper()
	asTenant(t, db, tenantID, func(tx *sql.Tx) {
		if _, err := tx.Exec(
			`INSERT INTO tags (tenant_id, name) VALUES ($1, $2)`,
			tenantID, name,
		); err != nil {
			t.Fatalf("seed tag %q for %s: %v", name, tenantID, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed: %v", err)
		}
	})
}

func containsRLS(s string) bool {
	// "row-level security policy" appears in the canonical pq error
	// message; "violates" precedes it. Either substring is enough.
	for _, sub := range []string{"row-level security", "violates"} {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

