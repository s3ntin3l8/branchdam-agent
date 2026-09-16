package resolve

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitSQLiteWritableModeIsForcedReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolve.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE fixture (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), "file:"+path+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.db.ExecContext(context.Background(), "INSERT INTO fixture VALUES (1)"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("writable SQLite URL was not forced read-only: %v", err)
	}
}

func TestSchemaProbeRejectsMissingResolveTables(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.CheckSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "schema incompatible") {
		t.Fatalf("missing Resolve schema accepted: %v", err)
	}
}
