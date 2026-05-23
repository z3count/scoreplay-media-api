package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/scoreplay/media-api/internal/domain"
	"github.com/scoreplay/media-api/internal/port"
)

// MediaRepo implements port.MediaRepository backed by a PostgreSQL database.
//
// Media creation is inherently transactional: we must insert the media row
// AND all its tag associations atomically. If any tag ID is invalid or the
// transaction fails, everything is rolled back. The caller (service layer)
// is responsible for cleaning up the associated file on disk when this happens.
type MediaRepo struct {
	db *sql.DB
}

// NewMediaRepo creates a new MediaRepo with the given database connection pool.
func NewMediaRepo(db *sql.DB) *MediaRepo {
	return &MediaRepo{db: db}
}

// Create inserts a new media record and associates it with the given tags in a
// single database transaction.
//
// Transaction flow:
//  1. BEGIN transaction
//  2. INSERT INTO media (...) RETURNING id, created_at
//  3. For each tag ID: INSERT INTO media_tags (media_id, tag_id)
//  4. SELECT tags for the new media (to populate the response)
//  5. COMMIT
//
// If any step fails, the transaction is rolled back. Specifically:
//   - If a tag ID references a non-existent tag, the FK constraint will fail.
//     We detect this and return domain.ErrNotFound so the service layer knows
//     the input was invalid (not a server error).
//   - If the database connection is lost mid-transaction, the driver auto-rolls back.
//
// Corner cases handled:
//   - Duplicate tag IDs in the input: the composite PK on media_tags prevents
//     duplicates. We deduplicate in the service layer before reaching here, but
//     the DB constraint is a safety net.
//   - Empty tagIDs slice: the media is created with no tag associations (valid).
//   - Very large number of tags: we use individual INSERTs rather than a batch
//     for simplicity. For 1000+ tags per media, a batch approach (COPY or
//     unnest) would be more efficient, but that's an unlikely scenario.
func (r *MediaRepo) Create(ctx context.Context, media *domain.Media, tagIDs []uuid.UUID) (*domain.Media, error) {
	err := withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		// Step 1: Insert the media record, stamped with the tenant.
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO media (tenant_id, name, media_type, file_path, original_name, file_size)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING id, created_at`,
			tenantID, media.Name, string(media.Type), media.FilePath, media.OriginalName, media.FileSize,
		).Scan(&media.ID, &media.CreatedAt); err != nil {
			return fmt.Errorf("insert media: %w", err)
		}

		// Step 2: Insert tag associations. Each tag must belong to the same
		// tenant — both the explicit WHERE EXISTS and the RLS policy block
		// cross-tenant pairs.
		for _, tagID := range tagIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO media_tags (tenant_id, media_id, tag_id)
				 SELECT $1, $2, $3
				 WHERE EXISTS (SELECT 1 FROM tags WHERE id = $3 AND tenant_id = $1)`,
				tenantID, media.ID, tagID,
			); err != nil {
				return fmt.Errorf("associate tag %s: %w", tagID, err)
			}
		}

		// Step 3: Fetch the tags to populate the response.
		// We re-read from the DB rather than trusting the input to ensure consistency.
		tags, err := queryMediaTags(ctx, tx, tenantID, media.ID)
		if err != nil {
			return fmt.Errorf("query media tags: %w", err)
		}
		media.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}
	return media, nil
}

// GetByID retrieves a media item by its UUID, including all associated tags.
//
// Uses a single query with LEFT JOIN to fetch the media and its tags in one
// round-trip to the database (avoiding N+1 queries).
//
// Returns domain.ErrNotFound if no media with the given ID exists. The caller
// can rely on errors.Is(err, domain.ErrNotFound) to distinguish between "not found"
// and genuine database errors.
//
// Corner cases:
//   - Media with no tags: the LEFT JOIN ensures the media row is still returned;
//     the Tags slice will be empty.
//   - Deleted tags (via CASCADE): if a tag was deleted after being associated
//     with a media, the media_tags row is also deleted (ON DELETE CASCADE),
//     so it simply won't appear in the result.
func (r *MediaRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Media, error) {
	var media domain.Media
	err := withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		// A cross-tenant request for a real id is treated as 404, not 403 —
		// leaking "this id exists but isn't yours" would itself be an
		// enumeration vector.
		err := tx.QueryRowContext(ctx,
			`SELECT id, name, media_type, file_path, original_name, file_size, created_at
			 FROM media WHERE id = $1 AND tenant_id = $2`,
			id, tenantID,
		).Scan(&media.ID, &media.Name, &media.Type, &media.FilePath, &media.OriginalName, &media.FileSize, &media.CreatedAt)
		if err == sql.ErrNoRows {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get media: %w", err)
		}
		tags, err := queryMediaTags(ctx, tx, tenantID, media.ID)
		if err != nil {
			return fmt.Errorf("query media tags: %w", err)
		}
		media.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &media, nil
}

// queryMediaTags fetches all tags associated with a media item within a transaction.
// This is used during Create to populate the response without an additional round-trip
// after the commit. Filtered by tenant_id for defence in depth — the caller has
// already verified ownership via the media row, but a stale media_tags from a
// schema bug shouldn't leak through.
func queryMediaTags(ctx context.Context, tx *sql.Tx, tenantID, mediaID uuid.UUID) ([]domain.Tag, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.name, t.created_at
		 FROM tags t
		 INNER JOIN media_tags mt ON mt.tag_id = t.id
		 WHERE mt.media_id = $1 AND mt.tenant_id = $2
		 ORDER BY t.name ASC`,
		mediaID, tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanTags(rows)
}

// scanTags iterates over a *sql.Rows result set and returns a slice of Tag structs.
func scanTags(rows *sql.Rows) ([]domain.Tag, error) {
	var tags []domain.Tag
	for rows.Next() {
		var t domain.Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if tags == nil {
		tags = []domain.Tag{}
	}
	return tags, nil
}

// List returns up to limit media items ordered by (created_at DESC, id DESC)
// using keyset pagination. When cursor is non-nil, only rows strictly older
// than the cursor position are returned via the tuple comparison
// `(created_at, id) < (cursor.created_at, cursor.id)`. The compound index
// `idx_media_cursor` lets PostgreSQL serve the page with a single range scan.
//
// We fetch one extra row beyond limit to detect whether more pages exist;
// if present, the last in-page row's key becomes the next cursor and the
// extra row is dropped. No COUNT(*) is performed.
//
// Multi-tag AND semantics: when filter.TagIDs or filter.TagNames contains
// N entries, the result is media tagged with ALL N tags. This is implemented
// with GROUP BY m.id HAVING COUNT(DISTINCT ...) = N, which lets Postgres
// reuse the same join shape for N=1 and N>1. The service layer is
// responsible for deduplicating the slices so the count predicate matches.
func (r *MediaRepo) List(ctx context.Context, limit int, cursor *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
	var mediaList []*domain.Media
	var next *port.MediaCursor
	err := withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		ml, n, err := r.listInTx(ctx, tx, tenantID, limit, cursor, filter)
		if err != nil {
			return err
		}
		mediaList = ml
		next = n
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return mediaList, next, nil
}

// listInTx contains the actual List query logic. It is extracted so the
// outer function stays a thin closure over withTenantTx — the query
// builder is long enough that nesting it inside the closure made the
// surrounding scope hard to read.
func (r *MediaRepo) listInTx(ctx context.Context, tx *sql.Tx, tenantID uuid.UUID, limit int, cursor *port.MediaCursor, filter port.MediaFilter) ([]*domain.Media, *port.MediaCursor, error) {
	var (
		joins   []string
		wheres  []string
		groupBy string
		having  string
		args    []interface{}
	)
	// placeholder appends v to args and returns the corresponding $N token.
	placeholder := func(v interface{}) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	// Tenant scope is the first WHERE — keeps the (tenant_id, created_at DESC, id DESC)
	// composite index usable for keyset scans.
	wheres = append(wheres, "m.tenant_id = "+placeholder(tenantID))

	switch {
	case len(filter.TagIDs) > 0:
		// Convert []uuid.UUID → []string because lib/pq doesn't recognize
		// uuid.UUID directly; the SQL casts the text array back to uuid[].
		idStrs := make([]string, len(filter.TagIDs))
		for i, id := range filter.TagIDs {
			idStrs[i] = id.String()
		}
		joins = append(joins, "INNER JOIN media_tags mt ON mt.media_id = m.id")
		wheres = append(wheres, "mt.tag_id = ANY("+placeholder(pq.Array(idStrs))+"::uuid[])")
		groupBy = " GROUP BY m.id "
		having = "HAVING COUNT(DISTINCT mt.tag_id) = " + placeholder(len(filter.TagIDs))
	case len(filter.TagNames) > 0:
		// Two-hop join: media → media_tags → tags. tags.name is UNIQUE so
		// each name resolves to at most one tag.
		joins = append(joins,
			"INNER JOIN media_tags mt ON mt.media_id = m.id",
			"INNER JOIN tags t ON t.id = mt.tag_id",
		)
		wheres = append(wheres, "t.name = ANY("+placeholder(pq.Array(filter.TagNames))+"::text[])")
		groupBy = " GROUP BY m.id "
		having = "HAVING COUNT(DISTINCT t.id) = " + placeholder(len(filter.TagNames))
	}

	if cursor != nil {
		wheres = append(wheres, fmt.Sprintf("(m.created_at, m.id) < (%s, %s)",
			placeholder(cursor.CreatedAt), placeholder(cursor.ID)))
	}

	var sb strings.Builder
	sb.WriteString(`SELECT m.id, m.name, m.media_type, m.file_path, m.original_name, m.file_size, m.created_at
		FROM media m`)
	for _, j := range joins {
		sb.WriteByte(' ')
		sb.WriteString(j)
	}
	if len(wheres) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(wheres, " AND "))
	}
	if groupBy != "" {
		sb.WriteString(groupBy)
		sb.WriteString(having)
	}
	sb.WriteString(" ORDER BY m.created_at DESC, m.id DESC LIMIT ")
	sb.WriteString(placeholder(limit + 1))

	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list media: %w", err)
	}
	defer rows.Close()

	mediaList := make([]*domain.Media, 0, limit+1)
	for rows.Next() {
		var m domain.Media
		if err := rows.Scan(&m.ID, &m.Name, &m.Type, &m.FilePath, &m.OriginalName, &m.FileSize, &m.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan media row: %w", err)
		}
		mediaList = append(mediaList, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate media rows: %w", err)
	}

	var next *port.MediaCursor
	if len(mediaList) > limit {
		last := mediaList[limit-1]
		next = &port.MediaCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		mediaList = mediaList[:limit]
	}

	// Batch-load tags for the in-page media items in a single query (no N+1).
	if len(mediaList) > 0 {
		mediaIDs := make([]string, len(mediaList))
		mediaMap := make(map[uuid.UUID]*domain.Media, len(mediaList))

		for i, m := range mediaList {
			mediaIDs[i] = m.ID.String()
			mediaMap[m.ID] = m
			m.Tags = []domain.Tag{} // Avoid JSON null for media with no tags.
		}

		tagQuery := `
			SELECT mt.media_id, t.id, t.name, t.created_at
			FROM tags t
			INNER JOIN media_tags mt ON t.id = mt.tag_id
			WHERE mt.media_id = ANY($1::uuid[]) AND mt.tenant_id = $2
			ORDER BY t.name ASC`

		tagRows, err := tx.QueryContext(ctx, tagQuery, pq.Array(mediaIDs), tenantID)
		if err != nil {
			return nil, nil, fmt.Errorf("batch query tags: %w", err)
		}
		defer tagRows.Close()

		for tagRows.Next() {
			var mediaID uuid.UUID
			var t domain.Tag
			if err := tagRows.Scan(&mediaID, &t.ID, &t.Name, &t.CreatedAt); err != nil {
				return nil, nil, fmt.Errorf("scan tag row: %w", err)
			}
			if m, exists := mediaMap[mediaID]; exists {
				m.Tags = append(m.Tags, t)
			}
		}

		if err := tagRows.Err(); err != nil {
			return nil, nil, fmt.Errorf("iterate tag rows: %w", err)
		}
	}

	return mediaList, next, nil
}

// Delete removes a media record from the database and returns its file_path
// so the caller can clean up the associated file from storage.
//
// Uses DELETE ... RETURNING to fetch the file path in a single query.
// The media_tags junction rows are deleted automatically via ON DELETE CASCADE.
//
// Returns domain.ErrNotFound if no media with the given ID exists.
func (r *MediaRepo) Delete(ctx context.Context, id uuid.UUID) (string, error) {
	var filePath string
	err := withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		err := tx.QueryRowContext(ctx,
			`DELETE FROM media WHERE id = $1 AND tenant_id = $2 RETURNING file_path`,
			id, tenantID,
		).Scan(&filePath)
		if err == sql.ErrNoRows {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete media: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return filePath, nil
}

// AttachTags inserts (media_id, tag_id) rows for every tag in tagIDs,
// ignoring associations that already exist via ON CONFLICT DO NOTHING.
//
// A pre-flight EXISTS check on the media id lets us return ErrNotFound
// before doing any work — the alternative (relying on the FK failure)
// also fires for invalid tag ids, and we want to map those two cases to
// different HTTP statuses (404 vs 400).
//
// The INSERT … SELECT unnest(…) shape is one round-trip for any tagIDs
// length. lib/pq doesn't know about uuid.UUID directly, so we render the
// slice as []string and cast back to uuid[] in SQL — same trick used in
// the cursor list query.
func (r *MediaRepo) AttachTags(ctx context.Context, mediaID uuid.UUID, tagIDs []uuid.UUID) error {
	return withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM media WHERE id = $1 AND tenant_id = $2)`,
			mediaID, tenantID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check media exists: %w", err)
		}
		if !exists {
			return domain.ErrNotFound
		}

		if len(tagIDs) == 0 {
			return nil
		}

		idStrs := make([]string, len(tagIDs))
		for i, id := range tagIDs {
			idStrs[i] = id.String()
		}

		// INSERT only the (media_id, tag_id) pairs where the tag actually
		// belongs to the same tenant. Tag IDs that don't exist (or belong to
		// a different tenant) are dropped by the WHERE clause; we count how
		// many were inserted to detect that case and return ErrValidation.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO media_tags (tenant_id, media_id, tag_id)
			 SELECT $1, $2, t.id
			 FROM tags t
			 WHERE t.id = ANY($3::uuid[]) AND t.tenant_id = $1
			 ON CONFLICT (media_id, tag_id) DO NOTHING`,
			tenantID, mediaID, pq.Array(idStrs),
		)
		if err != nil {
			return fmt.Errorf("attach tags: %w", err)
		}
		if rows, _ := res.RowsAffected(); rows < int64(len(tagIDs)) {
			var validCount int
			if cErr := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM tags WHERE id = ANY($1::uuid[]) AND tenant_id = $2`,
				pq.Array(idStrs), tenantID,
			).Scan(&validCount); cErr != nil {
				return fmt.Errorf("attach tags validation: %w", cErr)
			}
			if validCount < len(tagIDs) {
				return fmt.Errorf("%w: one or more tag IDs do not exist", domain.ErrValidation)
			}
		}
		return nil
	})
}

// DetachTag removes one (media_id, tag_id) pair from media_tags. The tag
// itself is untouched.
//
// Idempotent: removing a row that doesn't exist is not an error — DELETE
// is naturally retry-safe and we don't want clients to special-case races
// where someone else just unlinked the same pair.
//
// We still do an EXISTS check on the media so the client gets a 404 when
// the *media* id is wrong (vs. silently succeeding on a typo).
func (r *MediaRepo) DetachTag(ctx context.Context, mediaID, tagID uuid.UUID) error {
	return withTenantTx(ctx, r.db, func(tx *sql.Tx, tenantID uuid.UUID) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM media WHERE id = $1 AND tenant_id = $2)`,
			mediaID, tenantID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check media exists: %w", err)
		}
		if !exists {
			return domain.ErrNotFound
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM media_tags WHERE media_id = $1 AND tag_id = $2 AND tenant_id = $3`,
			mediaID, tagID, tenantID,
		); err != nil {
			return fmt.Errorf("detach tag: %w", err)
		}
		return nil
	})
}
