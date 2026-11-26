package queue

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenReadOnlyDoesNotMigrateOrWrite pins the MCP path: a reader must
// not bump user_version and must not touch the file's mtime. Open on the
// same v3 database would migrate it to the current schema.
func TestOpenReadOnlyDoesNotMigrateOrWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3_queue.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply v%d migration: %v", m.fromVersion, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	s, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Errorf("user_version = %d, want 3 (read-only open must not migrate)", version)
	}
	if _, err := s.db.Exec(`CREATE TABLE should_fail (id INTEGER)`); err == nil {
		t.Error("read-only connection accepted a write")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime changed from %s to %s", before.ModTime(), after.ModTime())
	}
	var versionAfter int
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if err := ro.QueryRow("PRAGMA user_version").Scan(&versionAfter); err != nil || versionAfter != 3 {
		t.Fatalf("user_version after close = %d, err = %v, want 3", versionAfter, err)
	}
}

// TestOpenReadOnlySeesWriterWAL is the other half of the MCP promise: a
// second process can read queue.db while the tray still holds it open
// (WAL, including the -shm file).
func TestOpenReadOnlySeesWriterWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if err := writer.InsertPending(ctx, sampleRecord("wal-1", "card.jpg")); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	counts, err := reader.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.AwaitingUpload != 1 {
		t.Fatalf("counts = %+v, want the writer's pending row", counts)
	}
	got, ok, err := reader.ByNodeUUID(ctx, "wal-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.SourcePath != "card.jpg" {
		t.Fatalf("row ok=%v source=%q", ok, got.SourcePath)
	}
}
