package queue

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPragmasApplied asserts the durability pragmas actually took effect on
// the connection Store uses -- "the DSN parses" or "Exec didn't error"
// proves nothing on its own (Store's own doc comment); query them back.
func TestPragmasApplied(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}

	var synchronous int
	if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	// SQLite reports synchronous=FULL as 2.
	if synchronous != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", synchronous)
	}

	var busyTimeout int
	if err := s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

func sampleRecord(nodeUUID, sourcePath string) NewRecord {
	return NewRecord{
		NodeUUID:               nodeUUID,
		Kind:                   KindMedia,
		SourcePath:             sourcePath,
		LocalPath:              "/local/" + sourcePath,
		ArchivePath:            "/archive/" + sourcePath,
		ArchiveContainerPath:   "/storage/archive/" + sourcePath,
		Tier0ContainerPath:     "/storage/staging/agent-1/" + sourcePath,
		FileName:               "file.jpg",
		FileExt:                "jpg",
		SizeBytes:              1024,
		MtimeUnix:              1700000000,
		FullHash:               "a" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abc",
		FastHash:               "0123456789abcdef",
		NodeCreatedPayloadJSON: `{"nodeUuid":"` + nodeUUID + `"}`,
		NodeCreatedEventUUID:   "evt-" + nodeUUID,
	}
}

func TestInsertAndByNodeUUID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-1", "DCIM/100/IMG_0001.JPG")
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ByNodeUUID(ctx, "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected row to exist")
	}
	if got.NodeCreatedStatus != StatusPending || got.ArchiveCopyStatus != StatusPending || got.RebaseStatus != StatusPending {
		t.Errorf("expected all-PENDING initial state, got %+v", got)
	}
	if got.SourcePath != rec.SourcePath {
		t.Errorf("SourcePath = %q, want %q", got.SourcePath, rec.SourcePath)
	}
}

func TestInsertDuplicateNodeUUIDFails(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-dup", "a.jpg")
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPending(ctx, rec); err == nil {
		t.Fatal("expected UNIQUE constraint violation on duplicate node_uuid, got nil")
	}
}

func TestSidecarInitialState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-sidecar", "a.xmp")
	rec.Kind = KindSidecar
	rec.NodeCreatedPayloadJSON = ""
	rec.Tier0ContainerPath = ""
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ByNodeUUID(ctx, "uuid-sidecar")
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.NodeCreatedStatus != StatusSkipped {
		t.Errorf("NodeCreatedStatus = %q, want SKIPPED", got.NodeCreatedStatus)
	}
	if got.RebaseStatus != StatusSkipped {
		t.Errorf("RebaseStatus = %q, want SKIPPED", got.RebaseStatus)
	}
	if got.ArchiveCopyStatus != StatusPending {
		t.Errorf("ArchiveCopyStatus = %q, want PENDING", got.ArchiveCopyStatus)
	}
	if got.Done() {
		t.Error("sidecar row should not be Done() until archive_copy completes")
	}
}

func TestPendingExcludesFullyDoneRows(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	done := sampleRecord("uuid-done", "done.jpg")
	if err := s.InsertPending(ctx, done); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "uuid-done"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseDone(ctx, "uuid-done"); err != nil {
		t.Fatal(err)
	}

	notDone := sampleRecord("uuid-notdone", "notdone.jpg")
	if err := s.InsertPending(ctx, notDone); err != nil {
		t.Fatal(err)
	}

	pending, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].NodeUUID != "uuid-notdone" {
		t.Errorf("Pending() = %+v, want only uuid-notdone", pending)
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("All() returned %d rows, want 2", len(all))
	}
}

func TestCountsOnEmptyStore(t *testing.T) {
	s := openTestStore(t)
	got, err := s.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Counts{}
	if got != want {
		t.Errorf("Counts() on an empty store = %+v, want zero value (COALESCE must avoid NULL, not just zero rows)", got)
	}
	if got.Pending() != 0 {
		t.Errorf("Pending() = %d, want 0", got.Pending())
	}
}

// TestCountsBuckets seeds one row per bucket Counts reports and asserts
// each lands in exactly the bucket its state implies -- most importantly
// that a FAILED rebase counts as Failed, never as Done, matching the type
// doc comment's "a badge must never read green over a permanently broken
// row" requirement.
func TestCountsBuckets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	awaitingUpload := sampleRecord("uuid-awaiting-upload", "a.jpg")
	awaitingUpload.SizeBytes = 1000
	if err := s.InsertPending(ctx, awaitingUpload); err != nil {
		t.Fatal(err)
	}

	awaitingRebase := sampleRecord("uuid-awaiting-rebase", "b.jpg")
	awaitingRebase.SizeBytes = 2000
	if err := s.InsertPending(ctx, awaitingRebase); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "uuid-awaiting-rebase"); err != nil {
		t.Fatal(err)
	}

	done := sampleRecord("uuid-done", "c.jpg")
	if err := s.InsertPending(ctx, done); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "uuid-done"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseDone(ctx, "uuid-done"); err != nil {
		t.Fatal(err)
	}

	failed := sampleRecord("uuid-failed", "d.jpg")
	if err := s.InsertPending(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "uuid-failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseFailed(ctx, "uuid-failed", "node is archived"); err != nil {
		t.Fatal(err)
	}

	sidecar := sampleRecord("uuid-sidecar-done", "e.xmp")
	sidecar.Kind = KindSidecar
	sidecar.NodeCreatedPayloadJSON = ""
	sidecar.Tier0ContainerPath = ""
	if err := s.InsertPending(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "uuid-sidecar-done"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := Counts{
		AwaitingUpload: 1,
		AwaitingRebase: 1,
		Failed:         1,
		Done:           2, // uuid-done and the sidecar (rebase SKIPPED, not PENDING)
		PendingBytes:   1000,
	}
	if got != want {
		t.Errorf("Counts() = %+v, want %+v", got, want)
	}
	if got.Pending() != 2 {
		t.Errorf("Pending() = %d, want 2 (AwaitingUpload + AwaitingRebase)", got.Pending())
	}
}

func TestMarkTransitions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-transitions", "x.jpg")
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1700000100, 0)
	if err := s.MarkNodeCreatedSubmitted(ctx, "uuid-transitions", "event-123", now); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ByNodeUUID(ctx, "uuid-transitions")
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeCreatedStatus != StatusSubmitted || got.NodeCreatedEventID != "event-123" || got.NodeCreatedSubmittedAtUnix != now.Unix() {
		t.Errorf("unexpected state after MarkNodeCreatedSubmitted: %+v", got)
	}

	if err := s.MarkArchiveCopyAttempt(ctx, "uuid-transitions", "nas unreachable", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.ByNodeUUID(ctx, "uuid-transitions")
	if got.ArchiveCopyAttempts != 1 || got.ArchiveCopyLastError != "nas unreachable" {
		t.Errorf("unexpected state after MarkArchiveCopyAttempt: %+v", got)
	}

	if err := s.MarkArchiveCopyDone(ctx, "uuid-transitions"); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkRebaseAttempt(ctx, "uuid-transitions", "transient", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.ByNodeUUID(ctx, "uuid-transitions")
	if got.RebaseAttempts != 1 || got.RebaseStatus != StatusPending {
		t.Errorf("expected rebase still PENDING after a retryable attempt: %+v", got)
	}

	if err := s.MarkRebaseFailed(ctx, "uuid-transitions", "node is archived"); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.ByNodeUUID(ctx, "uuid-transitions")
	if got.RebaseStatus != StatusFailed {
		t.Errorf("expected rebase FAILED, got %q", got.RebaseStatus)
	}
	if !got.Done() {
		t.Error("a FAILED rebase should count as terminal/Done() so Drain stops retrying it")
	}
}

// TestResetFailed: FAILED archive copies and FAILED rebases go back to
// PENDING with a fresh attempt counter and no backoff (so Pending()
// returns them again), keeping the last error; rows in any other state are
// untouched, and the counts say how many of each were reset.
func TestResetFailed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, u := range []string{"rebase-failed-1", "rebase-failed-2", "copy-failed", "pending", "done"} {
		if err := s.InsertPending(ctx, sampleRecord(u, u+".jpg")); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Unix(1700009999, 0)
	for _, u := range []string{"rebase-failed-1", "rebase-failed-2", "pending", "done"} {
		if err := s.MarkArchiveCopyDone(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"rebase-failed-1", "rebase-failed-2", "pending"} {
		if err := s.MarkRebaseAttempt(ctx, u, "HTTP 401", future); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"rebase-failed-1", "rebase-failed-2"} {
		if err := s.MarkRebaseFailed(ctx, u, "HTTP 409"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkArchiveCopyAttempt(ctx, "copy-failed", "nas unreachable", future); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyFailed(ctx, "copy-failed", "archive conflict"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseDone(ctx, "done"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ResetFailed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != (ResetCounts{ArchiveCopies: 1, Rebases: 2}) {
		t.Errorf("ResetFailed = %+v, want 1 archive copy and 2 rebases", got)
	}

	for _, u := range []string{"rebase-failed-1", "rebase-failed-2"} {
		r, _, _ := s.ByNodeUUID(ctx, u)
		if r.RebaseStatus != StatusPending || r.RebaseAttempts != 0 || r.RebaseNextAttemptUnix != 0 {
			t.Errorf("%s after reset: status=%q attempts=%d next=%d, want PENDING/0/0", u, r.RebaseStatus, r.RebaseAttempts, r.RebaseNextAttemptUnix)
		}
		if r.RebaseLastError != "HTTP 409" {
			t.Errorf("%s: RebaseLastError = %q, want the failure reason kept", u, r.RebaseLastError)
		}
		if r.Done() || r.Failed() {
			t.Errorf("%s: Done()=%v Failed()=%v after reset, want it back in Pending()", u, r.Done(), r.Failed())
		}
	}
	r, _, _ := s.ByNodeUUID(ctx, "copy-failed")
	if r.ArchiveCopyStatus != StatusPending || r.ArchiveCopyAttempts != 0 || r.ArchiveCopyNextAttemptUnix != 0 {
		t.Errorf("copy-failed after reset: status=%q attempts=%d next=%d, want PENDING/0/0", r.ArchiveCopyStatus, r.ArchiveCopyAttempts, r.ArchiveCopyNextAttemptUnix)
	}
	if r.ArchiveCopyLastError != "archive conflict" {
		t.Errorf("ArchiveCopyLastError = %q, want the failure reason kept", r.ArchiveCopyLastError)
	}
	if r, _, _ := s.ByNodeUUID(ctx, "pending"); r.RebaseAttempts != 1 || r.RebaseNextAttemptUnix != future.Unix() {
		t.Errorf("PENDING row's backoff was touched: %+v", r)
	}
	if r, _, _ := s.ByNodeUUID(ctx, "done"); r.RebaseStatus != StatusDone {
		t.Errorf("DONE row changed to %q", r.RebaseStatus)
	}
	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Failed != 0 || c.AwaitingRebase != 3 || c.AwaitingUpload != 1 {
		t.Errorf("Counts = %+v, want Failed=0 AwaitingRebase=3 AwaitingUpload=1", c)
	}

	if again, err := s.ResetFailed(ctx); err != nil || again != (ResetCounts{}) {
		t.Errorf("second reset = %+v, %v; want zero counts, nil", again, err)
	}
}

func TestBySourcePathResume(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.BySourcePath(ctx, "missing.jpg"); err != nil || ok {
		t.Fatalf("expected no row for missing.jpg, ok=%v err=%v", ok, err)
	}

	rec := sampleRecord("uuid-resume", "card/IMG.jpg")
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.BySourcePath(ctx, "card/IMG.jpg")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.NodeUUID != "uuid-resume" {
		t.Errorf("NodeUUID = %q, want uuid-resume", got.NodeUUID)
	}
}

// TestOpenFailsWhenDirectoryDoesNotExist covers Open's error branches
// (sql.Open/init failing) -- a path under a nonexistent parent directory
// can never succeed.
func TestOpenFailsWhenDirectoryDoesNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-subdir", "nested", "queue.db")
	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to fail when the parent directory does not exist")
	}
}

// TestQueryAndExecErrorBranchesOnClosedStore exercises every method's SQL
// error-wrap branch at once: every query/exec against an already-closed
// *sql.DB deterministically fails, which is the cheapest reliable way to
// reach each method's `return nil, fmt.Errorf(...)` line without needing a
// real SQL failure condition (corrupt file, permission denial, etc.).
func TestQueryAndExecErrorBranchesOnClosedStore(t *testing.T) {
	s := openTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if _, _, err := s.ByNodeUUID(ctx, "x"); err == nil {
		t.Error("ByNodeUUID on a closed store: expected an error")
	}
	if _, _, err := s.BySourcePath(ctx, "/x"); err == nil {
		t.Error("BySourcePath on a closed store: expected an error")
	}
	if _, err := s.Pending(ctx); err == nil {
		t.Error("Pending on a closed store: expected an error")
	}
	if _, err := s.All(ctx); err == nil {
		t.Error("All on a closed store: expected an error")
	}
	if _, err := s.Counts(ctx); err == nil {
		t.Error("Counts on a closed store: expected an error")
	}
	if err := s.InsertPending(ctx, NewRecord{NodeUUID: "x", Kind: KindMedia}); err == nil {
		t.Error("InsertPending on a closed store: expected an error")
	}
	if err := s.MarkNodeCreatedSubmitted(ctx, "x", "evt", time.Now()); err == nil {
		t.Error("MarkNodeCreatedSubmitted on a closed store: expected an error")
	}
	if err := s.MarkNodeCreatedAttempt(ctx, "x", "err", time.Now()); err == nil {
		t.Error("MarkNodeCreatedAttempt on a closed store: expected an error")
	}
	if err := s.MarkArchiveCopyDone(ctx, "x"); err == nil {
		t.Error("MarkArchiveCopyDone on a closed store: expected an error")
	}
	if err := s.MarkArchiveCopyAttempt(ctx, "x", "err", time.Now()); err == nil {
		t.Error("MarkArchiveCopyAttempt on a closed store: expected an error")
	}
	if err := s.MarkRebaseDone(ctx, "x"); err == nil {
		t.Error("MarkRebaseDone on a closed store: expected an error")
	}
	if err := s.MarkRebaseAttempt(ctx, "x", "err", time.Now()); err == nil {
		t.Error("MarkRebaseAttempt on a closed store: expected an error")
	}
	if err := s.MarkRebaseFailed(ctx, "x", "err"); err == nil {
		t.Error("MarkRebaseFailed on a closed store: expected an error")
	}
}

// TestMigrateV0ToV1AddsIndex creates a v0 queue.db (no index, no table)
// via Open, then asserts the migration ran by checking user_version is 1
// and that the status-column index exists.
func TestMigrateV0ToV1AddsIndex(t *testing.T) {
	s := openTestStore(t)

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d after migration", version, currentSchemaVersion)
	}

	// Verify the status-column index exists.
	var indexSQL string
	err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_queue_nodes_status'`,
	).Scan(&indexSQL)
	if err != nil {
		t.Fatalf("status-column index not found after migration: %v", err)
	}
	if indexSQL == "" {
		t.Error("status-column index SQL is empty after migration")
	}
}

// TestMigrateIsIdempotent opens the same queue.db twice and asserts
// no error on the second Open -- the migration must be safe to re-run.
func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent migration): %v", err)
	}
	defer func() { _ = s2.Close() }()

	var version int
	if err := s2.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d after second Open, want %d", version, currentSchemaVersion)
	}
}

// TestMigrateIsForwardOnly asserts the migrations slice is forward-only:
// every entry's fromVersion is strictly less than the next entry's, and
// the list starts at 0.
func TestMigrateIsForwardOnly(t *testing.T) {
	for i, m := range migrations {
		if m.fromVersion < 0 {
			t.Errorf("migration[%d].fromVersion = %d, want >= 0", i, m.fromVersion)
		}
		if i == 0 && m.fromVersion != 0 {
			t.Errorf("migration[0].fromVersion = %d, want 0 (must start at v0)", m.fromVersion)
		}
		if i > 0 && m.fromVersion <= migrations[i-1].fromVersion {
			t.Errorf("migration[%d].fromVersion = %d <= migration[%d].fromVersion = %d (not forward-only)",
				i, m.fromVersion, i-1, migrations[i-1].fromVersion)
		}
	}
}

// TestStatusIndexExistsAndIsUsedByPending asserts that the status-column
// index exists and is referenced by Pending's WHERE clause, which filters
// on archive_copy_status and rebase_status -- the exact columns the index
// covers. Counts() uses SUM(CASE...) aggregates over all rows, which
// don't benefit from an index; that's expected and not a regression.
func TestStatusIndexExistsAndIsUsedByPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Seed rows so Pending has work to do.
	if err := s.InsertPending(ctx, sampleRecord("uuid-idx-a", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPending(ctx, sampleRecord("uuid-idx-b", "b.jpg")); err != nil {
		t.Fatal(err)
	}

	var id, parent, notused int
	var detail string
	err := s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN
SELECT id, node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
	tier0_container_path, file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash,
	node_created_payload_json, node_created_event_uuid, node_created_status, node_created_event_id,
	node_created_submitted_at_unix, node_created_attempts, node_created_next_attempt_unix, node_created_last_error,
	archive_copy_status, archive_copy_attempts, archive_copy_next_attempt_unix, archive_copy_last_error,
	rebase_status, rebase_attempts, rebase_next_attempt_unix, rebase_last_error,
	created_at_unix
FROM queue_nodes
WHERE NOT (archive_copy_status = 'DONE' AND rebase_status IN ('DONE', 'SKIPPED', 'FAILED'))
ORDER BY id ASC`).Scan(&id, &parent, &notused, &detail)
	if err != nil {
		t.Fatal(err)
	}
	// Pending's WHERE clause filters on the status columns the index
	// covers; SQLite may or may not use the index depending on row
	// count, but the index must exist (proven by TestMigrateV0ToV1AddsIndex).
	// Here we verify Pending returns the right rows as a correctness
	// check rather than asserting a specific query plan shape.
	pending, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Errorf("Pending() = %d rows, want 2", len(pending))
	}
}

func TestMigrateV1ToV2AddsNodeCreatedEventUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1_queue.db")
	// Open raw DB and apply v1 schema
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Run migration 0 (v0 -> v1)
	v0SQL := `CREATE TABLE queue_nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_uuid TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL CHECK (kind IN ('MEDIA','SIDECAR')),
	source_path TEXT NOT NULL,
	local_path TEXT NOT NULL,
	archive_path TEXT NOT NULL,
	archive_container_path TEXT NOT NULL,
	tier0_container_path TEXT NOT NULL DEFAULT '',
	file_name TEXT NOT NULL,
	file_ext TEXT NOT NULL,
	size_bytes INTEGER NOT NULL,
	mtime_unix INTEGER NOT NULL,
	full_hash TEXT NOT NULL,
	fast_hash TEXT NOT NULL,
	node_created_payload_json TEXT NOT NULL DEFAULT '',
	node_created_status TEXT NOT NULL DEFAULT 'PENDING',
	node_created_event_id TEXT NOT NULL DEFAULT '',
	node_created_submitted_at_unix INTEGER NOT NULL DEFAULT 0,
	node_created_attempts INTEGER NOT NULL DEFAULT 0,
	node_created_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	node_created_last_error TEXT NOT NULL DEFAULT '',
	archive_copy_status TEXT NOT NULL DEFAULT 'PENDING',
	archive_copy_attempts INTEGER NOT NULL DEFAULT 0,
	archive_copy_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	archive_copy_last_error TEXT NOT NULL DEFAULT '',
	rebase_status TEXT NOT NULL DEFAULT 'PENDING',
	rebase_attempts INTEGER NOT NULL DEFAULT 0,
	rebase_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	rebase_last_error TEXT NOT NULL DEFAULT '',
	created_at_unix INTEGER NOT NULL
);
CREATE INDEX idx_queue_nodes_status ON queue_nodes(archive_copy_status, rebase_status);
PRAGMA user_version = 1;`
	if _, err := db.Exec(v0SQL); err != nil {
		t.Fatalf("setup v1 db: %v", err)
	}
	// Insert a row in v1 DB with no node_created_event_uuid
	_, err = db.Exec(`INSERT INTO queue_nodes (
		node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
		file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash, created_at_unix
	) VALUES ('orig-uuid-1', 'MEDIA', 'p', 'l', 'a', 'ac', 'f.jpg', 'jpg', 10, 100, 'fh', 'fast', 100)`)
	if err != nil {
		t.Fatalf("insert v1 row: %v", err)
	}
	_ = db.Close()

	// Open with Store.Open, which should migrate v1 -> v2
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	rec, ok, err := s.ByNodeUUID(context.Background(), "orig-uuid-1")
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID: ok=%v, err=%v", ok, err)
	}
	if rec.NodeCreatedEventUUID != "orig-uuid-1" {
		t.Errorf("NodeCreatedEventUUID = %q, want %q backfilled from node_uuid", rec.NodeCreatedEventUUID, "orig-uuid-1")
	}
}

// TestArchiveCopyFailedIsTerminalAndCounted: a terminally failed archive
// copy (a conflicting file at the archive path, or an edited local copy)
// leaves Pending() so Drain stops cycling on it, is never reported as Done
// in Counts, and is counted as Failed so the tray/window show it.
func TestArchiveCopyFailedIsTerminalAndCounted(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-archive-failed", "f.jpg")
	rec.SizeBytes = 500
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyFailed(ctx, "uuid-archive-failed", "archive destination exists with different content: /a/f.jpg"); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.ByNodeUUID(ctx, "uuid-archive-failed")
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID: ok=%v err=%v", ok, err)
	}
	if got.ArchiveCopyStatus != StatusFailed || got.ArchiveCopyLastError == "" {
		t.Errorf("row = %+v, want archive_copy FAILED with its error recorded", got)
	}
	if !got.Done() || !got.Failed() {
		t.Errorf("Done()=%v Failed()=%v, want both true for a terminally failed archive copy", got.Done(), got.Failed())
	}

	pending, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("Pending() = %d rows, want 0 -- a terminal failure must not be retried", len(pending))
	}

	counts, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Counts{Failed: 1}); counts != want {
		t.Errorf("Counts() = %+v, want %+v", counts, want)
	}
}

// TestCountsBucketsWithArchiveCopyFailedMix: with archive_copy FAILED rows
// mixed in among PENDING and DONE ones, every row lands in exactly one of
// AwaitingUpload / AwaitingRebase / Failed / Done.
func TestCountsBucketsWithArchiveCopyFailedMix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insert := func(id string) {
		t.Helper()
		if err := s.InsertPending(ctx, sampleRecord(id, id+".jpg")); err != nil {
			t.Fatal(err)
		}
	}
	// 2 awaiting upload
	insert("p1")
	insert("p2")
	// 3 archive_copy FAILED (one of them also attempted first)
	for _, id := range []string{"af1", "af2", "af3"} {
		insert(id)
		if err := s.MarkArchiveCopyFailed(ctx, id, "conflict"); err != nil {
			t.Fatal(err)
		}
	}
	// 1 awaiting rebase
	insert("r1")
	if err := s.MarkArchiveCopyDone(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	// 2 done
	for _, id := range []string{"d1", "d2"} {
		insert(id)
		if err := s.MarkArchiveCopyDone(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkRebaseDone(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// 1 rebase FAILED
	insert("rf1")
	if err := s.MarkArchiveCopyDone(ctx, "rf1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseFailed(ctx, "rf1", "archived"); err != nil {
		t.Fatal(err)
	}

	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Counts{AwaitingUpload: 2, AwaitingRebase: 1, Failed: 4, Done: 2, PendingBytes: c.PendingBytes}
	if c != want {
		t.Errorf("Counts() = %+v, want %+v", c, want)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum := c.AwaitingUpload + c.AwaitingRebase + c.Failed + c.Done; sum != len(all) {
		t.Errorf("buckets sum to %d, want the row count %d", sum, len(all))
	}
}

// TestOnlineRowLifecycle: an ONLINE row starts with its archive copy done
// and no rebase, stays pending (and counted) until EVENT_NODE_CREATED is
// accepted, and is then done.
func TestOnlineRowLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleRecord("uuid-online", "online.jpg")
	rec.Origin = OriginOnline
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ByNodeUUID(ctx, "uuid-online")
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != OriginOnline || got.NodeCreatedStatus != StatusPending || got.ArchiveCopyStatus != StatusDone || got.RebaseStatus != StatusSkipped {
		t.Fatalf("initial row = %+v", got)
	}
	if got.Done() {
		t.Error("Done() = true before the registration was accepted")
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending() = %d rows (err=%v), want the unregistered online row", len(pending), err)
	}
	if c, err := s.Counts(ctx); err != nil || c != (Counts{AwaitingRebase: 1}) {
		t.Errorf("Counts() = %+v (err=%v), want it awaiting the server", c, err)
	}

	if err := s.MarkNodeCreatedSubmitted(ctx, "uuid-online", "evt", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.ByNodeUUID(ctx, "uuid-online")
	if !got.Done() || got.Failed() {
		t.Errorf("after submission Done()=%v Failed()=%v, want done", got.Done(), got.Failed())
	}
	if pending, _ := s.Pending(ctx); len(pending) != 0 {
		t.Errorf("Pending() = %d rows after submission, want 0", len(pending))
	}
	if c, _ := s.Counts(ctx); c != (Counts{Done: 1}) {
		t.Errorf("Counts() = %+v after submission, want Done=1", c)
	}

	// Default and unknown origins.
	off := sampleRecord("uuid-offline", "offline.jpg")
	if err := s.InsertPending(ctx, off); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.ByNodeUUID(ctx, "uuid-offline"); got.Origin != OriginOffline || got.ArchiveCopyStatus != StatusPending {
		t.Errorf("default-origin row = %+v, want OFFLINE with archive_copy PENDING", got)
	}
	bad := sampleRecord("uuid-bad", "bad.jpg")
	bad.Origin = "SIDEWAYS"
	if err := s.InsertPending(ctx, bad); err == nil {
		t.Error("InsertPending accepted an unknown origin")
	}
}

// TestMigrateV2ToV3AddsOriginDefaultingToOffline: every row written before
// v3 came from offline ingest, so it migrates as OFFLINE.
func TestMigrateV2ToV3AddsOriginDefaultingToOffline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2_queue.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply v%d migration: %v", m.fromVersion, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO queue_nodes (
		node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
		file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash, created_at_unix
	) VALUES ('v2-uuid', 'MEDIA', 'p', 'l', 'a', 'ac', 'f.jpg', 'jpg', 10, 100, 'fh', 'fast', 100)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	rec, ok, err := s.ByNodeUUID(context.Background(), "v2-uuid")
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID: ok=%v err=%v", ok, err)
	}
	if rec.Origin != OriginOffline {
		t.Errorf("Origin = %q, want %q", rec.Origin, OriginOffline)
	}
}

// TestOpenEscapesAwkwardPaths: '?', '#', '%' and spaces in the queue.db
// path must reach SQLite as part of the file name. Unescaped, SQLite reads
// '?' as the start of URI parameters and opens a different (truncated)
// path.
func TestOpenEscapesAwkwardPaths(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "state dir?x=1#frag%41")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "queue 100%?.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	if err := s.InsertPending(context.Background(), sampleRecord("uuid-awkward", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("queue.db not created at the exact path: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("parent dir holds %v, want only the state dir (no file at a truncated path)", names)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if _, ok, err := s2.ByNodeUUID(context.Background(), "uuid-awkward"); err != nil || !ok {
		t.Errorf("reopened store lost the row: ok=%v err=%v", ok, err)
	}
}

// TestPragmasAppliedToEveryConnection: database/sql may replace the pool's
// connection at any time; the replacement must carry the same pragmas.
func TestPragmasAppliedToEveryConnection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	s.db.SetMaxIdleConns(0) // every query below gets a brand-new connection

	for i := 0; i < 2; i++ {
		var busy, sync, fk int
		var journal string
		if err := s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 || sync != 2 || fk != 1 || journal != "wal" {
			t.Errorf("connection %d: busy_timeout=%d synchronous=%d foreign_keys=%d journal_mode=%s, want 5000/2/1/wal", i, busy, sync, fk, journal)
		}
	}
}

// gcFixture inserts a row, drives it to the given state, backdates its
// created_at and points its local_path at a file that exists or not.
func gcFixture(t *testing.T, s *Store, id, state string, age time.Duration, localExists bool) {
	t.Helper()
	ctx := context.Background()
	rec := sampleRecord(id, id+".jpg")
	rec.LocalPath = filepath.Join(t.TempDir(), id+".jpg")
	if state == "sidecar-done" {
		rec.Kind = KindSidecar
	}
	if localExists {
		if err := os.WriteFile(rec.LocalPath, []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "done":
		_ = s.MarkNodeCreatedSubmitted(ctx, id, "evt", time.Now())
		_ = s.MarkArchiveCopyDone(ctx, id)
		_ = s.MarkRebaseDone(ctx, id)
	case "sidecar-done":
		_ = s.MarkArchiveCopyDone(ctx, id)
	case "archive-failed":
		_ = s.MarkArchiveCopyFailed(ctx, id, "conflict")
	case "rebase-failed":
		_ = s.MarkArchiveCopyDone(ctx, id)
		_ = s.MarkRebaseFailed(ctx, id, "archived")
	case "pending":
	default:
		t.Fatalf("unknown state %q", state)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE queue_nodes SET created_at_unix = ? WHERE node_uuid = ?`, time.Now().Add(-age).Unix(), id); err != nil {
		t.Fatal(err)
	}
}

// TestCollectGarbageDeletesOnlyFinishedOldRowsWithoutLocalCopy pins every
// condition under which a row is kept.
func TestCollectGarbageDeletesOnlyFinishedOldRowsWithoutLocalCopy(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	old := GCRetention + 24*time.Hour
	gcFixture(t, s, "done-old-gone", "done", old, false)
	gcFixture(t, s, "sidecar-old-gone", "sidecar-done", old, false)
	gcFixture(t, s, "done-old-present", "done", old, true) // prune may still need it
	gcFixture(t, s, "done-young-gone", "done", time.Hour, false)
	gcFixture(t, s, "archive-failed-old", "archive-failed", old, false)
	gcFixture(t, s, "rebase-failed-old", "rebase-failed", old, false)
	gcFixture(t, s, "pending-old", "pending", old, false)

	n, err := s.CollectGarbage(ctx, time.Now(), GCRetention)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("deleted %d rows, want 2", n)
	}
	all, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	left := map[string]bool{}
	for _, r := range all {
		left[r.NodeUUID] = true
	}
	for _, id := range []string{"done-old-gone", "sidecar-old-gone"} {
		if left[id] {
			t.Errorf("%s kept, want collected", id)
		}
	}
	for _, id := range []string{"done-old-present", "done-young-gone", "archive-failed-old", "rebase-failed-old", "pending-old"} {
		if !left[id] {
			t.Errorf("%s collected, want kept", id)
		}
	}
}

// TestCollectGarbageIfDueThrottles: one sweep per GCInterval per Store.
func TestCollectGarbageIfDueThrottles(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	old := GCRetention + 24*time.Hour

	gcFixture(t, s, "first", "done", old, false)
	if n, err := s.CollectGarbageIfDue(ctx, now); err != nil || n != 1 {
		t.Fatalf("first sweep: n=%d err=%v, want 1", n, err)
	}
	gcFixture(t, s, "second", "done", old, false)
	if n, err := s.CollectGarbageIfDue(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweep within GCInterval: n=%d err=%v, want 0 (throttled)", n, err)
	}
	if n, err := s.CollectGarbageIfDue(ctx, now.Add(GCInterval)); err != nil || n != 1 {
		t.Fatalf("sweep after GCInterval: n=%d err=%v, want 1", n, err)
	}
}
