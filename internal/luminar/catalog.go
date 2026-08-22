// Package luminar reads Skylum Luminar's catalog.db to recover edit->source
// relationships and emits them to branchDAM as EVENT_EDGE_ATTACHED events.
//
// Luminar's catalog schema is not publicly documented (see
// docs/luminar-catalog.md for the research trail and confidence level), so
// the query used to extract edit->source pairs is deliberately isolated in
// query.go and overridable at runtime via --query-file, rather than baked in
// as a confident constant. Everything in this file is schema-agnostic: it
// only knows how to open the database safely and run whatever query it is
// given.
package luminar

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Catalog is a read-only handle on a Luminar catalog.db.
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
// backs `luminar-sync --dump-schema`: since Luminar's schema is
// undocumented, the fastest way for an operator with a real catalog.db to
// correct query.go's guessed query is to run --dump-schema against their own
// catalog and compare the real table/column names against
// DefaultEditSourceQuery.
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

// EditSourcePair is one edit->source relationship recovered from the
// catalog: SourcePath is the original/master image Luminar imported,
// EditPath is the path of the edited/exported output derived from it, and
// SourceRowID is the catalog's own row identifier for the source side
// (whatever query.go's query aliased as source_row_id), stamped into the
// emitted edge's evidenceJson so a future data-correction migration (see
// docs/luminar-catalog.md) can find every edge produced by a given schema
// mapping version.
type EditSourcePair struct {
	SourcePath  string
	EditPath    string
	SourceRowID string
	EditRowID   string
}

// EditSourcePairs runs query against the catalog and scans the result into
// EditSourcePair values. query must select exactly four columns in this
// order: source_path, edit_path, source_row_id, edit_row_id (see
// DefaultEditSourceQuery in query.go for the reference shape). Row
// identifiers are read as text via a generic scan so this works whether the
// catalog's primary keys are integers or strings.
func (c *Catalog) EditSourcePairs(ctx context.Context, query string) ([]EditSourcePair, error) {
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("luminar: run edit-source query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("luminar: read result columns: %w", err)
	}
	if len(cols) != 4 {
		return nil, fmt.Errorf("luminar: edit-source query must select exactly 4 columns (source_path, edit_path, source_row_id, edit_row_id), got %d: %v", len(cols), cols)
	}

	var out []EditSourcePair
	for rows.Next() {
		var p EditSourcePair
		if err := rows.Scan(&p.SourcePath, &p.EditPath, &p.SourceRowID, &p.EditRowID); err != nil {
			return nil, fmt.Errorf("luminar: scan edit-source row: %w", err)
		}
		if p.SourcePath == "" || p.EditPath == "" {
			// A query that returns an empty path is almost certainly
			// selecting the wrong column against the guessed schema --
			// surfacing it loudly here is cheaper than a silently-skipped
			// pair three layers up in the syncer.
			return nil, fmt.Errorf("luminar: edit-source query returned an empty source_path or edit_path (row source_row_id=%q edit_row_id=%q) -- likely a schema mismatch, see docs/luminar-catalog.md", p.SourceRowID, p.EditRowID)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("luminar: iterate edit-source rows: %w", err)
	}
	return out, nil
}
