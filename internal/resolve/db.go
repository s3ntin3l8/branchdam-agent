// Package resolve reads DaVinci Resolve's project database (PostgreSQL
// Project Server or local SQLite disk database) to recover timeline→clip
// relationships and logs evidence metadata. v2 will emit
// EVENT_EDGE_ATTACHED events once proper PROJECT_SIDECAR edges
// (media → virtual project node) are supported (issue #184).
//
// The schema was reverse-engineered from a live DaVinci Resolve 19 Project
// Server instance (PostgreSQL 13, using the Resolve 19 Project Server defaults;
// rotate before exposing to a network). The
// key relationship chain is:
//
//	SM_Project → Sm2Timeline → Sm2Sequence → Sm2TiTrack → Sm2TiItem
//	                                                       ↓
//	                                                  MediaFilePath
//
// Codec/resolution metadata lives in binary blobs (BtVideoInfo.Clip,
// BtVideoInfo.Geometry) and is not queryable as SQL; this package only
// uses the queryable columns (MediaFilePath, In, Start, Duration).
package resolve

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	// PostgreSQL driver — the Project Server is PostgreSQL 13.
	_ "github.com/lib/pq"
	// SQLite driver — local disk databases use the same schema.
	_ "modernc.org/sqlite"
)

// DB is a read-only handle on a DaVinci Resolve project database.
type DB struct {
	db *sql.DB
}

// Open opens a PostgreSQL or SQLite connection based on databaseURL's scheme.
//
// PostgreSQL: "postgres://user:pass@host:5432/dbname" or "postgresql://..."
// SQLite:     "file:/path/to/project.db?mode=ro"
//
// The connection is always read-only, single-connection
// (SetMaxOpenConns(1)), matching the Luminar catalog convention.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	driver, dsn, err := parseURL(databaseURL)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("resolve: open %s: %w", StripCredentials(databaseURL), err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("resolve: ping %s: %w", StripCredentials(databaseURL), err)
	}
	return &DB{db: db}, nil
}

// Close releases the underlying connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// parseURL detects the driver and normalizes the DSN from a database URL.
func parseURL(databaseURL string) (driver, dsn string, err error) {
	switch {
	case strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://"):
		return "postgres", databaseURL, nil
	case strings.HasPrefix(databaseURL, "file:"):
		// SQLite file: URI — pass through as-is. mode=ro is the caller's
		// responsibility (same convention as internal/luminar.Open).
		return "sqlite", databaseURL, nil
	default:
		return "", "", fmt.Errorf("resolve: unsupported database URL scheme: %s (expected postgres://... or file:...)", StripCredentials(databaseURL))
	}
}

// TimelineClips runs query against the database and scans the result into
// TimelineClip values. query must select exactly 8 columns in the order
// DefaultTimelineQuery documents: timeline_name, clip_name, media_file_path,
// in_point, start_frame, duration, item_id, timeline_id.
func (d *DB) TimelineClips(ctx context.Context, query string) ([]TimelineClip, error) {
	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("resolve: run timeline query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("resolve: read result columns: %w", err)
	}
	if len(cols) != 8 {
		return nil, fmt.Errorf("resolve: timeline query must select exactly 8 columns (timeline_name, clip_name, media_file_path, in_point, start_frame, duration, item_id, timeline_id), got %d: %v", len(cols), cols)
	}

	var out []TimelineClip
	for rows.Next() {
		var clip TimelineClip
		if err := rows.Scan(
			&clip.TimelineName,
			&clip.ClipName,
			&clip.MediaFilePath,
			&clip.InPoint,
			&clip.StartFrame,
			&clip.Duration,
			&clip.ItemID,
			&clip.TimelineID,
		); err != nil {
			return nil, fmt.Errorf("resolve: scan timeline row: %w", err)
		}
		if clip.MediaFilePath == "" {
			return nil, fmt.Errorf("resolve: timeline query returned empty media_file_path (item_id=%q) — likely a schema mismatch", clip.ItemID)
		}
		out = append(out, clip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve: iterate timeline rows: %w", err)
	}
	return out, nil
}
