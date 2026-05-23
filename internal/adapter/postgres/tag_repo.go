// Package postgres implements the TagRepository interface using PostgreSQL.
//
// This is a "driven adapter" in hexagonal architecture: it is called by the
// service layer through the port.TagRepository interface. The service layer
// has no knowledge that PostgreSQL is being used — it only knows about the
// interface contract.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// uniqueViolation is the PostgreSQL SQLSTATE code for a unique-constraint
// violation. Detecting this lets the repo map a rename collision to
// domain.ErrConflict so the HTTP layer can return 409.
const uniqueViolation = "23505"

// TagRepo implements port.TagRepository backed by a PostgreSQL database.
//
// It uses raw SQL queries (no ORM) for full control over query structure
// and performance. This is important for the future "search media by tag"
// feature, which will require optimized JOIN queries.
type TagRepo struct {
	db *sql.DB
}

// NewTagRepo creates a new TagRepo with the given database connection pool.
//
// The caller is responsible for managing the lifecycle of the *sql.DB (opening,
// closing, connection pool settings). This follows the dependency injection
// pattern: the repo does not own its dependencies.
func NewTagRepo(db *sql.DB) *TagRepo {
	return &TagRepo{db: db}
}

// CreateOrGet inserts a new tag with the given name, or returns the existing tag
// if a tag with that name already exists. This provides idempotent tag creation.
//
// Implementation detail: uses PostgreSQL's INSERT ... ON CONFLICT ... DO NOTHING
// followed by a SELECT. We use a two-statement approach within a single function
// rather than DO UPDATE because:
//  1. DO NOTHING + SELECT is cleaner when we truly don't want to modify existing rows.
//  2. DO UPDATE SET name = EXCLUDED.name (a no-op update) would still increment
//     the row's xmin, causing unnecessary WAL writes and bloat.
//
// The returned bool indicates whether a new tag was created (true) or an existing
// one was returned (false).
//
// Corner cases handled:
//   - Concurrent inserts with the same name: the UNIQUE constraint ensures only
//     one succeeds; the other falls through to the SELECT.
//   - Empty or whitespace-only names: should be rejected at the service layer
//     before reaching this method.
func (r *TagRepo) CreateOrGet(ctx context.Context, name string) (domain.Tag, bool, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return domain.Tag{}, false, err
	}

	// Attempt to insert. If a conflict on (tenant_id, name) occurs, do nothing.
	// RETURNING gives us the row only if an insert actually happened.
	var tag domain.Tag
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO tags (tenant_id, name) VALUES ($1, $2)
		 ON CONFLICT (tenant_id, name) DO NOTHING
		 RETURNING id, name, created_at`,
		tenantID, name,
	).Scan(&tag.ID, &tag.Name, &tag.CreatedAt)

	if err == nil {
		// Insert succeeded: new tag was created.
		return tag, true, nil
	}

	if err != sql.ErrNoRows {
		// Genuine database error (connection lost, syntax error, etc.).
		return domain.Tag{}, false, fmt.Errorf("insert tag: %w", err)
	}

	// sql.ErrNoRows means the ON CONFLICT fired (tag already exists).
	// Fetch the existing tag by (tenant_id, name).
	err = r.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM tags WHERE tenant_id = $1 AND name = $2`,
		tenantID, name,
	).Scan(&tag.ID, &tag.Name, &tag.CreatedAt)
	if err != nil {
		return domain.Tag{}, false, fmt.Errorf("select existing tag: %w", err)
	}

	return tag, false, nil
}

// List returns up to limit tags ordered by (name ASC, id ASC) using keyset
// pagination. When cursor is non-nil, only rows strictly after the cursor are
// returned; the tuple comparison `(name, id) > (cursor.name, cursor.id)` lets
// PostgreSQL serve the page with a single range scan over the index on name.
//
// We fetch one extra row beyond limit to detect whether more pages exist.
// If we got the extra, it is dropped from the response and the last in-page
// row's key becomes the next cursor.
//
// No COUNT(*) is performed: cursor pagination intentionally avoids the
// linear-time scan that offset-based pagination required.
func (r *TagRepo) List(ctx context.Context, limit int, cursor *port.TagCursor) ([]domain.Tag, *port.TagCursor, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	var rows *sql.Rows
	if cursor == nil {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, name, created_at FROM tags
			 WHERE tenant_id = $1
			 ORDER BY name ASC, id ASC
			 LIMIT $2`,
			tenantID, limit+1,
		)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, name, created_at FROM tags
			 WHERE tenant_id = $1 AND (name, id) > ($2, $3)
			 ORDER BY name ASC, id ASC
			 LIMIT $4`,
			tenantID, cursor.Name, cursor.ID, limit+1,
		)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	tags := make([]domain.Tag, 0, limit+1)
	for rows.Next() {
		var t domain.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan tag: %w", err)
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate tags: %w", err)
	}

	var next *port.TagCursor
	if len(tags) > limit {
		last := tags[limit-1]
		next = &port.TagCursor{Name: last.Name, ID: last.ID}
		tags = tags[:limit]
	}
	return tags, next, nil
}

// Rename updates a tag's name and returns the updated row in one round-trip
// via UPDATE … RETURNING.
//
// Behavior:
//   - Tag not found → domain.ErrNotFound (sql.ErrNoRows from RETURNING).
//   - New name collides with another tag → unique-violation 23505 →
//     domain.ErrConflict.
//   - New name equals the current name → no-op, returns the existing row;
//     PostgreSQL does not raise UNIQUE for an UPDATE that doesn't change
//     the key value.
//
// The caller (service) is responsible for sanitizing the name before
// calling this method.
func (r *TagRepo) Rename(ctx context.Context, id uuid.UUID, name string) (domain.Tag, error) {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return domain.Tag{}, err
	}
	var tag domain.Tag
	err = r.db.QueryRowContext(ctx,
		`UPDATE tags SET name = $1 WHERE id = $2 AND tenant_id = $3
		 RETURNING id, name, created_at`,
		name, id, tenantID,
	).Scan(&tag.ID, &tag.Name, &tag.CreatedAt)

	if err == sql.ErrNoRows {
		return domain.Tag{}, domain.ErrNotFound
	}
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == uniqueViolation {
			return domain.Tag{}, fmt.Errorf("%w: a tag with that name already exists", domain.ErrConflict)
		}
		return domain.Tag{}, fmt.Errorf("rename tag: %w", err)
	}
	return tag, nil
}

// Delete removes a tag by ID. media_tags rows referencing the tag are
// removed automatically via ON DELETE CASCADE in the schema.
//
// Returns domain.ErrNotFound if no row matched. We rely on RowsAffected
// rather than a separate SELECT because DELETE is already atomic and
// reporting "deleted nothing" needs no extra round-trip.
func (r *TagRepo) Delete(ctx context.Context, id uuid.UUID) error {
	tenantID, err := port.TenantIDFromContext(ctx)
	if err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM tags WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete tag rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}
