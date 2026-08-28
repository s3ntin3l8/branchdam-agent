// Package luminar reads Luminar Neo's catalog file to recover edit->source
// relationships and emits them to branchDAM as EVENT_EDGE_ATTACHED events.
//
// The row-extraction query is verified against a real Luminar Neo catalog
// (db_version 155 -- see docs/luminar-catalog.md) but the catalog itself
// stores NO relational edit->source lineage; a derived file is only ever
// recoverable by filename convention (derive.go's PairDerivatives). The
// query is still isolated in query.go and overridable at runtime via
// --query-file, so a schema difference in another Luminar Neo version can be
// corrected without a code change. Everything in this file only knows the
// fixed 7-column row shape Images requires, never which real tables/columns
// produce it -- that stays in query.go.
package luminar

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Catalog is a read-only handle on a Luminar Neo catalog file.
type Catalog struct {
	db *sql.DB
}

// Open opens path as file:<path>?mode=ro -- never ?immutable=1. This was
// verified empirically (see catalog_wal_test.go): against a WAL-mode
// database, ?immutable=1 can silently ignore the -wal file and return stale
// data with no error, which for a catalog someone has open in Luminar right
// now means silently missing the newest edit. ?mode=ro reads the WAL
// correctly and is still safely read-only -- SQLite's own mode=ro refuses
// any write at the VFS layer, it does not rely on the caller only issuing
// SELECTs.
//
// A relative path is accepted by database/sql's file: URI construction, but
// callers should pass an absolute path -- SQLite resolves a relative path in
// the URI relative to the process's current working directory, not the
// catalog's own location, which is rarely what's intended for a path a user
// typed on a command line.
func Open(ctx context.Context, path string) (*Catalog, error) {
	// dsn below is built by string concatenation, not URI-encoding path --
	// a path containing '?' or '#' would either inject a second query
	// parameter (SQLite's URI parser keeps the FIRST occurrence of a
	// duplicate key, so "file:catalog.db?immutable=1?mode=ro" opens
	// immutable=1, exactly the mode this package exists to never use) or
	// get silently truncated at a '#' fragment. Reject both outright rather
	// than pass through a DSN whose parameters aren't what the caller
	// thinks they are.
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("luminar: catalog path %q must not contain '?' or '#' -- it is concatenated into a file: URI and would be misinterpreted as query parameters", path)
	}
	dsn := fmt.Sprintf("file:%s?mode=ro", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("luminar: open %s: %w", path, err)
	}
	// Read-only access to a file someone else (Luminar) may be writing to
	// concurrently has no reason to hold more than one connection, and a
	// single connection makes the WAL-visibility behavior this package
	// depends on easier to reason about.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("luminar: ping %s: %w", path, err)
	}
	return &Catalog{db: db}, nil
}

// Close releases the underlying connection.
func (c *Catalog) Close() error {
	return c.db.Close()
}

// SchemaObject is one row of sqlite_master, as returned by DumpSchema.
type SchemaObject struct {
	Type string // "table", "index", "view", "trigger"
	Name string
	SQL  string // the object's original CREATE statement; empty for some internal objects
}

// DumpSchema returns every object in the catalog's sqlite_master table. This
// backs `luminar-sync --dump-schema`: the fastest way for an operator to
// confirm DefaultCatalogQuery still matches their own Luminar Neo version
// (or to build a --query-file override if it doesn't) is to run
// --dump-schema against their own catalog and diff the real table/column
// names against query.go.
func (c *Catalog) DumpSchema(ctx context.Context) ([]SchemaObject, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("luminar: query sqlite_master: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SchemaObject
	for rows.Next() {
		var o SchemaObject
		if err := rows.Scan(&o.Type, &o.Name, &o.SQL); err != nil {
			return nil, fmt.Errorf("luminar: scan sqlite_master row: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("luminar: iterate sqlite_master: %w", err)
	}
	return out, nil
}

// CatalogImage is one row of DefaultCatalogQuery's output: everything known
// about a single catalog image, before any edit->source inference happens.
// Pairing (derive.go's PairDerivatives) works entirely off these values --
// the catalog itself carries no relational lineage, see query.go's doc
// comment.
type CatalogImage struct {
	ImageID     string // the catalog's own row identifier (images._id_int_64)
	VolumeMount string // filesystem mount point; empty if unresolvable
	DirPath     string // containing directory, relative to VolumeMount
	FileName    string // bare filename -- never a path in this schema
	Trashed     bool
	CameraModel string
	CaptureTime int64
}

// FullPath joins VolumeMount, DirPath, and FileName into an absolute path in
// the same slash-separated form nodeindex.Resolve expects to match verbatim
// (internal/nodeindex.Resolve does no normalization). ok is false when
// VolumeMount is empty -- no absolute path can be built, so nodeindex could
// never match it regardless.
//
// path.Join also NORMALIZES its result (collapses "//", cleans "..",
// strips a trailing "/"), which nodeindex.Resolve's verbatim map lookup does
// not do on the other side. The two sides only ever agree if the node-index
// file was itself built from already-normalized paths -- true for every
// path form observed in the real catalog this was verified against, but an
// index built by hand with a trailing slash or a doubled separator would
// silently never match here. Worth checking first if -node-index entries
// mysteriously never resolve.
//
// Only macOS path assembly (a POSIX volume mount plus '/'-joined
// sub-paths) has been verified against a real catalog; a Windows Luminar
// Neo catalog's volumes.info_wide_ch shape is unobserved and may need a
// different join here.
func (c CatalogImage) FullPath() (string, bool) {
	if c.VolumeMount == "" {
		return "", false
	}
	return path.Join(c.VolumeMount, c.DirPath, c.FileName), true
}

// Images runs query against the catalog and scans the result into
// CatalogImage values. query must select exactly 7 columns in the order
// DefaultCatalogQuery documents: image_id, volume_mount, dir_path,
// file_name, trashed, camera_model, capture_time.
func (c *Catalog) Images(ctx context.Context, query string) ([]CatalogImage, error) {
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("luminar: run catalog query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("luminar: read result columns: %w", err)
	}
	if len(cols) != 7 {
		return nil, fmt.Errorf("luminar: catalog query must select exactly 7 columns (image_id, volume_mount, dir_path, file_name, trashed, camera_model, capture_time), got %d: %v", len(cols), cols)
	}

	var out []CatalogImage
	for rows.Next() {
		var (
			img         CatalogImage
			trashed     sql.NullInt64
			volumeMount sql.NullString
			cameraModel sql.NullString
			captureTime sql.NullInt64
		)
		if err := rows.Scan(&img.ImageID, &volumeMount, &img.DirPath, &img.FileName, &trashed, &cameraModel, &captureTime); err != nil {
			return nil, fmt.Errorf("luminar: scan catalog row: %w", err)
		}
		if img.FileName == "" {
			// An empty filename is almost certainly the wrong column against
			// a changed schema -- surfacing it loudly here is cheaper than a
			// silently-mispaired image three layers up in the syncer.
			return nil, fmt.Errorf("luminar: catalog query returned an empty file_name (row image_id=%q) -- likely a schema mismatch, see docs/luminar-catalog.md", img.ImageID)
		}
		img.VolumeMount = volumeMount.String
		img.Trashed = trashed.Int64 != 0
		img.CameraModel = cameraModel.String
		img.CaptureTime = captureTime.Int64
		out = append(out, img)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("luminar: iterate catalog rows: %w", err)
	}
	return out, nil
}
