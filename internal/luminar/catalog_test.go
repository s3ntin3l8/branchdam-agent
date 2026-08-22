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
