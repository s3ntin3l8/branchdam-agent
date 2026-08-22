package luminar

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// createFixtureCatalog builds a catalog.db at dir/catalog.db shaped like
// DefaultEditSourceQuery's guessed schema (ZASSET/ZEDIT, CoreData-style) and
// returns its path. This fixture is intentionally generated from readable SQL
// in this test file, not committed as a binary .db under testdata/ -- the
// guessed schema stays visible in source control, and there's no stale
// binary to keep in sync with query.go's comments.
//
// This fixture proves the reader's plumbing (open, query, scan) against a
// schema shaped like the guess -- it does NOT validate the guess itself
// against a real Luminar catalog, which nothing in this repo can do. See
// docs/luminar-catalog.md's confidence section.
func createFixtureCatalog(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()

	const schema = `
CREATE TABLE ZASSET (
	Z_PK INTEGER PRIMARY KEY,
	ZFILEPATH TEXT
);
CREATE TABLE ZEDIT (
	Z_PK INTEGER PRIMARY KEY,
	ZASSET INTEGER,
	ZEXPORTPATH TEXT
);
`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create fixture schema: %v", err)
	}

	seed := []struct {
		assetPK int
		path    string
		editPK  int
		export  string
		hasEdit bool
	}{
		{1, "/masters/DSC_0001.NEF", 1, "/exports/DSC_0001-edit.jpg", true},
		{2, "/masters/DSC_0002.NEF", 2, "", true},  // edit exists but never exported -- must be excluded
		{3, "/masters/DSC_0003.NEF", 0, "", false}, // no edit at all
	}
	for _, s := range seed {
		if _, err := db.Exec(`INSERT INTO ZASSET (Z_PK, ZFILEPATH) VALUES (?, ?)`, s.assetPK, s.path); err != nil {
			t.Fatalf("seed asset %d: %v", s.assetPK, err)
		}
		if s.hasEdit {
			var exportVal any
			if s.export != "" {
				exportVal = s.export
			}
			if _, err := db.Exec(`INSERT INTO ZEDIT (Z_PK, ZASSET, ZEXPORTPATH) VALUES (?, ?, ?)`, s.editPK, s.assetPK, exportVal); err != nil {
				t.Fatalf("seed edit for asset %d: %v", s.assetPK, err)
			}
		}
	}

	return path
}

func TestEditSourcePairs(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	pairs, err := cat.EditSourcePairs(ctx, DefaultEditSourceQuery)
	if err != nil {
		t.Fatalf("EditSourcePairs: %v", err)
	}

	if len(pairs) != 1 {
		t.Fatalf("expected exactly 1 pair (only the exported edit), got %d: %+v", len(pairs), pairs)
	}
	got := pairs[0]
	if got.SourcePath != "/masters/DSC_0001.NEF" {
		t.Errorf("SourcePath = %q, want /masters/DSC_0001.NEF", got.SourcePath)
	}
	if got.EditPath != "/exports/DSC_0001-edit.jpg" {
		t.Errorf("EditPath = %q, want /exports/DSC_0001-edit.jpg", got.EditPath)
	}
	if got.SourceRowID != "1" {
		t.Errorf("SourceRowID = %q, want 1", got.SourceRowID)
	}
	if got.EditRowID != "1" {
		t.Errorf("EditRowID = %q, want 1", got.EditRowID)
	}
}

func TestEditSourcePairsRejectsWrongColumnCount(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	_, err = cat.EditSourcePairs(ctx, `SELECT ZFILEPATH FROM ZASSET`)
	if err == nil {
		t.Fatal("expected an error for a query returning the wrong column count, got nil")
	}
}

func TestDumpSchema(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	objs, err := cat.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("DumpSchema: %v", err)
	}
	var sawAsset, sawEdit bool
	for _, o := range objs {
		if o.Type == "table" && o.Name == "ZASSET" {
			sawAsset = true
		}
		if o.Type == "table" && o.Name == "ZEDIT" {
			sawEdit = true
		}
	}
	if !sawAsset || !sawEdit {
		t.Errorf("expected ZASSET and ZEDIT tables in schema dump, got %+v", objs)
	}
}

func TestOpenRejectsPathWithQueryOrFragmentCharacters(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{
		"/tmp/catalog.db?immutable=1",
		"/tmp/catalog.db#frag",
	} {
		if _, err := Open(ctx, bad); err == nil {
			t.Errorf("Open(%q): expected an error, got nil -- a '?'/'#' in the path can be misinterpreted as DSN query parameters", bad)
		}
	}
}

// TestEditSourcePairsRejectsEmptyPath covers the loud-failure branch for a
// query that returns a NULL/empty source_path or edit_path -- almost
// certainly the wrong column against the guessed schema. Surfacing this
// here, rather than silently skipping the row, is cheaper to debug than a
// missing pair discovered three layers up in the syncer.
func TestEditSourcePairsRejectsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	// Selects ZFILEPATH twice as a stand-in for edit_path, so edit_path is
	// never empty -- swap in an empty-string literal for source_path
	// instead to hit the loud-failure branch deterministically.
	_, err = cat.EditSourcePairs(ctx, `SELECT '' AS source_path, ZFILEPATH AS edit_path, Z_PK AS source_row_id, Z_PK AS edit_row_id FROM ZASSET LIMIT 1`)
	if err == nil {
		t.Fatal("expected an error for a query returning an empty source_path, got nil")
	}
}

// TestEditSourcePairsQueryError covers db.QueryContext's own error branch --
// a query referencing a table that doesn't exist in this catalog's schema
// (the most likely real-world cause: a guessed query against a Luminar
// version whose schema has since changed).
func TestEditSourcePairsQueryError(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	_, err = cat.EditSourcePairs(ctx, `SELECT a, b, c, d FROM ZNO_SUCH_TABLE`)
	if err == nil {
		t.Fatal("expected an error for a query against a nonexistent table, got nil")
	}
}

// TestDumpSchemaQueryUsesRealSQLiteMaster is a light sanity check that
// DumpSchema's SQL/type/name triples are non-degenerate for a CREATE
// TABLE statement, not just that the two known table names appear (already
// covered by TestDumpSchema) -- confirms the SQL column itself is
// populated, which --dump-schema's whole value proposition depends on.
func TestDumpSchemaQueryUsesRealSQLiteMaster(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)

	ctx := context.Background()
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	objs, err := cat.DumpSchema(ctx)
	if err != nil {
		t.Fatalf("DumpSchema: %v", err)
	}
	for _, o := range objs {
		if o.Type == "table" && o.Name == "ZASSET" {
			if o.SQL == "" {
				t.Error("ZASSET's SQL column is empty, want its CREATE TABLE statement")
			}
			return
		}
	}
	t.Fatal("ZASSET not found in schema dump")
}

func TestOpenNonexistentFile(t *testing.T) {
	// mode=ro against a file that doesn't exist must fail, not silently
	// create one -- that's the whole point of read-only access to a
	// third-party app's live database.
	ctx := context.Background()
	_, err := Open(ctx, filepath.Join(t.TempDir(), "does-not-exist.db"))
	if err == nil {
		t.Fatal("expected an error opening a nonexistent catalog read-only, got nil")
	}
}
