package luminar

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestModeROSeesLiveWAL is the AC-required proof for issue #6: against a
// WAL-mode database with a row still sitting in the -wal file (not yet
// checkpointed into the main database file), ?mode=ro sees it.
//
// The writer connection is deliberately kept open for the whole test rather
// than opened-write-closed: closing the last connection to a WAL-mode
// SQLite database triggers an automatic checkpoint, which would fold the
// -wal file back into the main file and defeat the entire point of this
// test (both mode=ro and immutable=1 would then see the row, for the wrong
// reason -- there'd be no WAL-only data left to disagree about). A held-open
// writer is also the realistic case this whole package exists for: a
// catalog someone has open and is actively editing in Luminar right now.
func TestModeROSeesLiveWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.db")
	ctx := context.Background()

	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	// A single, held-open connection: database/sql pools connections by
	// default, and an idle pooled connection can still be reaped/reopened
	// in ways that would checkpoint the WAL out from under this test.
	// SetMaxOpenConns(1) plus grabbing that one connection explicitly
	// (below) is what guarantees "the writer" means one specific, alive
	// SQLite connection for the test's whole duration.
	writer.SetMaxOpenConns(1)

	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire writer conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("set WAL mode: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, val TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO t (id, val) VALUES (1, 'hello')`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// Confirm the row is genuinely sitting in the WAL file and hasn't been
	// checkpointed into the main database file -- otherwise this test would
	// pass even with a driver that ignores WAL entirely.
	walPath := path + "-wal"
	fi, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat %s: %v (expected a non-empty -wal file with the uncheckpointed row)", walPath, err)
	}
	if fi.Size() == 0 {
		t.Fatalf("%s exists but is empty -- row may have been checkpointed already, test setup is unsound", walPath)
	}

	// --- mode=ro: the hard requirement (AC) ---
	roDB, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open mode=ro: %v", err)
	}
	defer func() { _ = roDB.Close() }()

	var count int
	if err := roDB.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("mode=ro: query row count: %v", err)
	}
	if count != 1 {
		t.Errorf("mode=ro: got %d row(s), want 1 -- mode=ro should see data still sitting in the WAL file", count)
	}

	// mode=ro must also refuse a write -- it's not just "happens to see the
	// WAL," it has to still be genuinely read-only at the VFS layer.
	if _, err := roDB.ExecContext(ctx, `INSERT INTO t (id, val) VALUES (2, 'nope')`); err == nil {
		t.Error("mode=ro: INSERT unexpectedly succeeded; mode=ro did not actually open read-only (DSN may not have been honored)")
	}

	// --- immutable=1: documented as silently stale against a live WAL,
	// verified during planning research. The exact failure shape (query
	// error vs. a stale 0-row read) is not hard-asserted, since either is
	// consistent with "immutable=1 does not see the live WAL" and a future
	// SQLite/driver version could change which one occurs without that
	// being a regression in anything this package controls. What IS
	// hard-asserted is that it must not see the row the way mode=ro does --
	// if it did, the documented rationale for Open() using mode=ro over
	// immutable=1 would no longer hold for this driver version, and that
	// needs to fail the build loudly, not scroll past in a log line nobody
	// reads.
	immutableDB, openErr := sql.Open("sqlite", "file:"+path+"?immutable=1")
	if openErr == nil {
		defer func() { _ = immutableDB.Close() }()
		var immutableCount int
		queryErr := immutableDB.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&immutableCount)
		switch {
		case queryErr != nil:
			t.Logf("immutable=1: query failed as expected for a live-WAL database: %v", queryErr)
		case immutableCount == 0:
			t.Logf("immutable=1: query succeeded but saw 0 rows -- confirms the documented stale-read behavior")
		default:
			t.Errorf("immutable=1: unexpectedly saw %d row(s) still sitting in the WAL -- this modernc.org/sqlite version no longer reproduces the stale-WAL behavior issue #6 documents as the reason Open() must never use immutable=1; re-check whether that rationale still holds", immutableCount)
		}
	} else {
		t.Logf("immutable=1: open failed outright against a live-WAL database: %v", openErr)
	}
}
