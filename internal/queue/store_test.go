package queue

import (
	"context"
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
