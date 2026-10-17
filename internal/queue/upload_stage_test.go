package queue

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func uploadRecord(nodeUUID, sourcePath string) NewRecord {
	rec := sampleRecord(nodeUUID, sourcePath)
	rec.Origin = OriginUpload
	rec.ArchivePath, rec.ArchiveContainerPath, rec.Tier0ContainerPath = "", "", ""
	rec.NodeCreatedPayloadJSON, rec.NodeCreatedEventUUID = "", ""
	rec.UploadName = "file.jpg"
	rec.CameraModel = "Canon EOS R5"
	rec.CaptureTimestamp = 1700000123
	rec.SourcePathHash = "sph-" + nodeUUID
	rec.LocalEditRoot = "/local"
	return rec
}

// TestMigrateV3ToV4AddsTheUploadStage: rows written before v4 are not upload
// rows, so they migrate with no upload step (SKIPPED) and every other column
// exactly as it was.
func TestMigrateV3ToV4AddsTheUploadStage(t *testing.T) {
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
	if _, err := db.Exec(`INSERT INTO queue_nodes (
		node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
		file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash, created_at_unix, origin
	) VALUES ('v3-uuid', 'MEDIA', 'p', 'l', 'a', 'ac', 'f.jpg', 'jpg', 10, 100, 'fh', 'fast', 100, 'ONLINE')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("user_version = %d, err = %v, want 4", version, err)
	}
	rec, ok, err := s.ByNodeUUID(context.Background(), "v3-uuid")
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID: ok=%v err=%v", ok, err)
	}
	if rec.UploadStatus != StatusSkipped || rec.Origin != OriginOnline || rec.MasterNodeUUID != "" || rec.ServerNodeUUID != "" {
		t.Errorf("migrated row = %+v", rec)
	}
	// And an UPLOAD row can now be inserted into the migrated database.
	if err := s.InsertPending(context.Background(), uploadRecord("up-1", "a.jpg")); err != nil {
		t.Fatalf("insert upload row after migration: %v", err)
	}
}

// TestMigrationRollsBackWhole: a migration that fails partway leaves the
// database at the old version with none of its changes, so the retry on the
// next Open is not a duplicate-column error.
func TestMigrationRollsBackWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.applyMigration(4, `ALTER TABLE queue_nodes ADD COLUMN scratch TEXT; THIS IS NOT SQL;`); err == nil {
		t.Fatal("applyMigration of broken SQL succeeded")
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("user_version = %d, err = %v, want it untouched at %d", version, err, currentSchemaVersion)
	}
	if _, err := s.db.Exec(`SELECT scratch FROM queue_nodes`); err == nil {
		t.Error("the failed migration's ALTER survived: the migration is not atomic")
	}
	_ = s.Close()
}

func TestUploadRowInitialStateAndLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertPending(ctx, uploadRecord("up-1", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	r, ok, err := s.ByNodeUUID(ctx, "up-1")
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID: ok=%v err=%v", ok, err)
	}
	if r.Origin != OriginUpload || r.UploadStatus != StatusPending || r.NodeCreatedStatus != StatusSkipped ||
		r.ArchiveCopyStatus != StatusSkipped || r.RebaseStatus != StatusSkipped || r.NodeCreatedEventUUID != "" {
		t.Fatalf("initial state = %+v", r)
	}
	if r.UploadName != "file.jpg" || r.CameraModel != "Canon EOS R5" || r.CaptureTimestamp != 1700000123 ||
		r.SourcePathHash != "sph-up-1" || r.LocalEditRoot != "/local" {
		t.Errorf("upload inputs not stored: %+v", r)
	}
	if r.Done() || r.Failed() {
		t.Errorf("Done=%v Failed=%v on a pending upload", r.Done(), r.Failed())
	}

	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("Pending = %d, err = %v, want the upload row", len(pending), err)
	}
	c, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.AwaitingUpload != 1 || c.AwaitingRebase != 0 || c.Failed != 0 || c.Done != 0 || c.PendingBytes != 1024 || c.Pending() != 1 {
		t.Errorf("counts while pending = %+v", c)
	}

	// A transient failure keeps it pending with a backoff.
	next := time.Now().Add(time.Minute)
	if err := s.MarkUploadAttempt(ctx, "up-1", "reset", next); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.ByNodeUUID(ctx, "up-1")
	if r.UploadAttempts != 1 || r.UploadLastError != "reset" || r.UploadNextAttemptUnix != next.Unix() || r.UploadStatus != StatusPending {
		t.Errorf("after attempt = %+v", r)
	}

	// Done records the server's answer and the actual local path.
	applied, err := s.MarkUploadDone(ctx, "up-1", "srv-1", "2026/a.jpg", "/local/2026/a.jpg")
	if err != nil || !applied {
		t.Fatalf("MarkUploadDone: applied=%v err=%v", applied, err)
	}
	r, _, _ = s.ByNodeUUID(ctx, "up-1")
	if r.UploadStatus != StatusDone || r.ServerNodeUUID != "srv-1" || r.ServerRelativePath != "2026/a.jpg" ||
		r.LocalPath != "/local/2026/a.jpg" || r.UploadLastError != "" {
		t.Errorf("after done = %+v", r)
	}
	if !r.Done() || r.Failed() {
		t.Errorf("Done=%v Failed=%v after upload", r.Done(), r.Failed())
	}
	if pending, _ := s.Pending(ctx); len(pending) != 0 {
		t.Errorf("Pending = %d after upload, want 0", len(pending))
	}
	c, _ = s.Counts(ctx)
	if c.AwaitingUpload != 0 || c.Done != 1 || c.Failed != 0 || c.PendingBytes != 0 {
		t.Errorf("counts after upload = %+v", c)
	}
}

// TestMarkUploadDoneIsACompareAndSet: Drain and a card re-insert's reconcile
// can both finish a row; only the first records it, and a late attempt or
// failure mark cannot undo it.
func TestMarkUploadDoneIsACompareAndSet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.InsertPending(ctx, uploadRecord("up-1", "a.jpg")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkUploadDone(ctx, "up-1", "first", "2026/first.jpg", "/local/first.jpg"); err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	if ok, err := s.MarkUploadDone(ctx, "up-1", "second", "2026/second.jpg", "/local/second.jpg"); err != nil || ok {
		t.Fatalf("second: ok=%v err=%v, want it refused", ok, err)
	}
	_ = s.MarkUploadAttempt(ctx, "up-1", "late", time.Now())
	_ = s.MarkUploadFailed(ctx, "up-1", "late")
	r, _, _ := s.ByNodeUUID(ctx, "up-1")
	if r.UploadStatus != StatusDone || r.ServerNodeUUID != "first" || r.LocalPath != "/local/first.jpg" || r.UploadAttempts != 0 {
		t.Errorf("row = %+v, want the first record untouched", r)
	}
}

func TestUploadFailedIsTerminalCountedAndResettable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"up-1", "up-2"} {
		if err := s.InsertPending(ctx, uploadRecord(id, id+".jpg")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkUploadFailed(ctx, "up-1", "hash refused"); err != nil {
		t.Fatal(err)
	}
	r, _, _ := s.ByNodeUUID(ctx, "up-1")
	if r.UploadStatus != StatusFailed || r.UploadLastError != "hash refused" || !r.Failed() || !r.Done() {
		t.Fatalf("failed row = %+v (Done=%v Failed=%v)", r, r.Done(), r.Failed())
	}
	if pending, _ := s.Pending(ctx); len(pending) != 1 || pending[0].NodeUUID != "up-2" {
		t.Errorf("Pending = %+v, want only up-2", pending)
	}
	c, _ := s.Counts(ctx)
	if c.Failed != 1 || c.Done != 0 || c.AwaitingUpload != 1 {
		t.Errorf("counts = %+v", c)
	}
	failed, err := s.FailedRows(ctx, 0)
	if err != nil || len(failed) != 1 || failed[0].NodeUUID != "up-1" {
		t.Errorf("FailedRows = %+v err = %v", failed, err)
	}

	// Reset puts exactly that row back, with a fresh attempt counter.
	counts, err := s.ResetFailedNode(ctx, "up-1")
	if err != nil || counts.Uploads != 1 || counts.ArchiveCopies != 0 || counts.Rebases != 0 {
		t.Fatalf("ResetFailedNode = %+v err = %v", counts, err)
	}
	r, _, _ = s.ByNodeUUID(ctx, "up-1")
	if r.UploadStatus != StatusPending || r.UploadAttempts != 0 || r.UploadNextAttemptUnix != 0 {
		t.Errorf("after reset = %+v", r)
	}
	if counts, err := s.ResetFailed(ctx); err != nil || counts.Uploads != 0 {
		t.Errorf("ResetFailed with nothing failed = %+v err = %v", counts, err)
	}
}

func TestPendingUploadLookups(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := uploadRecord("up-1", "card/IMG_1.CR3")
	rec.LocalPath = "/local/2026/IMG_1.CR3"
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// A different origin at the same source path never matches.
	other := sampleRecord("off-1", "card/IMG_1.CR3")
	if err := s.InsertPending(ctx, other); err != nil {
		t.Fatal(err)
	}

	if r, ok, err := s.PendingUploadByLocalPath(ctx, "/local/2026/IMG_1.CR3"); err != nil || !ok || r.NodeUUID != "up-1" {
		t.Errorf("PendingUploadByLocalPath = %+v ok=%v err=%v", r, ok, err)
	}
	if _, ok, _ := s.PendingUploadByLocalPath(ctx, "/local/missing"); ok {
		t.Error("PendingUploadByLocalPath matched a missing path")
	}
	if r, ok, err := s.UnfinishedUploadBySource(ctx, "card/IMG_1.CR3", strings.ToUpper(rec.FullHash)); err != nil || !ok || r.NodeUUID != "up-1" {
		t.Errorf("UnfinishedUploadBySource (hash case-insensitive) = %+v ok=%v err=%v", r, ok, err)
	}
	if _, ok, _ := s.UnfinishedUploadBySource(ctx, "card/IMG_1.CR3", "different"); ok {
		t.Error("UnfinishedUploadBySource matched a different hash")
	}

	if _, err := s.MarkUploadDone(ctx, "up-1", "srv", "2026/x", "/local/2026/IMG_1.CR3"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PendingUploadByLocalPath(ctx, "/local/2026/IMG_1.CR3"); ok {
		t.Error("a finished upload still reads as pending (by local path)")
	}
	if _, ok, _ := s.UnfinishedUploadBySource(ctx, "card/IMG_1.CR3", rec.FullHash); ok {
		t.Error("a finished upload still reads as pending (by source)")
	}
}

// TestCollectGarbageNeverCollectsAPendingUploadRow: the row is the agent's
// only record that this local copy still has to be uploaded -- however old
// it is, and even if its copy is gone.
func TestCollectGarbageNeverCollectsAPendingUploadRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := uploadRecord("up-old", "a.jpg")
	rec.LocalPath = filepath.Join(t.TempDir(), "gone.jpg") // never created
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	fin := uploadRecord("up-fin", "b.jpg")
	fin.LocalPath = filepath.Join(t.TempDir(), "gone-too.jpg")
	if err := s.InsertPending(ctx, fin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkUploadDone(ctx, "up-fin", "srv", "2026/b.jpg", fin.LocalPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE queue_nodes SET created_at_unix = ?`, time.Now().Add(-400*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	n, err := s.CollectGarbage(ctx, time.Now(), GCRetention)
	if err != nil || n != 1 {
		t.Fatalf("CollectGarbage = %d, err = %v, want only the finished row", n, err)
	}
	if _, ok, _ := s.ByNodeUUID(ctx, "up-old"); !ok {
		t.Error("a pending upload row was garbage-collected")
	}
	if _, ok, _ := s.ByNodeUUID(ctx, "up-fin"); ok {
		t.Error("a finished, old upload row with no local copy was kept")
	}
	_ = os.Remove(rec.LocalPath)
}

// TestInsertPendingRejectsAnUnknownOrigin keeps the allowlist honest now that
// there are three origins.
func TestInsertPendingRejectsAnUnknownOrigin(t *testing.T) {
	s := openTestStore(t)
	rec := uploadRecord("x", "a.jpg")
	rec.Origin = "NOPE"
	if err := s.InsertPending(context.Background(), rec); err == nil {
		t.Fatal("InsertPending accepted an unknown origin")
	}
}

// TestCollectGarbageKeepsAMasterWhileASidecarStillWaitsOnIt: Drain names the
// sidecar after the master's server-assigned path, which only the master's
// (finished, old) row remembers.
func TestCollectGarbageKeepsAMasterWhileASidecarStillWaitsOnIt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	master := uploadRecord("master", "a.jpg")
	master.LocalPath = filepath.Join(t.TempDir(), "gone.jpg") // never created
	if err := s.InsertPending(ctx, master); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkUploadDone(ctx, "master", "srv", "2026/a.jpg", master.LocalPath); err != nil {
		t.Fatal(err)
	}
	side := uploadRecord("side", "a.xmp")
	side.Kind = KindSidecar
	side.MasterNodeUUID = "master"
	side.LocalPath = filepath.Join(t.TempDir(), "gone.xmp")
	if err := s.InsertPending(ctx, side); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE queue_nodes SET created_at_unix = ?`, time.Now().Add(-400*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	if n, err := s.CollectGarbage(ctx, time.Now(), GCRetention); err != nil || n != 0 {
		t.Fatalf("CollectGarbage = %d, err = %v, want the master kept for its pending sidecar", n, err)
	}
	if _, err := s.MarkUploadDone(ctx, "side", "srv-2", "2026/a.xmp", side.LocalPath); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CollectGarbage(ctx, time.Now(), GCRetention); err != nil || n != 2 {
		t.Fatalf("CollectGarbage after the sidecar finished = %d, err = %v, want both collected", n, err)
	}
}

// TestUnfinishedUploadLookupsAndDoneIncludeAFailedRow: a row Drain gave up on
// (FAILED) is still resolved by a card re-insert that uploads the file
// itself -- found by path or by source+hash, and closable -- while ingest's
// own "already queued" check still only sees pending rows.
func TestUnfinishedUploadLookupsAndDoneIncludeAFailedRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := uploadRecord("up-1", "card/IMG_1.CR3")
	rec.LocalPath = "/local/2026/IMG_1.CR3"
	if err := s.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploadFailed(ctx, "up-1", "local copy unavailable"); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.PendingUploadByLocalPath(ctx, rec.LocalPath); ok {
		t.Error("PendingUploadByLocalPath matched a FAILED row")
	}
	if r, ok, err := s.UnfinishedUploadByLocalPath(ctx, rec.LocalPath); err != nil || !ok || r.NodeUUID != "up-1" {
		t.Errorf("UnfinishedUploadByLocalPath = %+v ok=%v err=%v, want the FAILED row", r, ok, err)
	}
	if r, ok, err := s.UnfinishedUploadBySource(ctx, "card/IMG_1.CR3", rec.FullHash); err != nil || !ok || r.NodeUUID != "up-1" {
		t.Errorf("UnfinishedUploadBySource = %+v ok=%v err=%v, want the FAILED row", r, ok, err)
	}

	if ok, err := s.MarkUploadDone(ctx, "up-1", "srv", "2026/x", "/local/2026/IMG_1.CR3"); err != nil || !ok {
		t.Fatalf("MarkUploadDone on a FAILED row: ok=%v err=%v", ok, err)
	}
	r, _, _ := s.ByNodeUUID(ctx, "up-1")
	if r.UploadStatus != StatusDone || r.Failed() || r.UploadLastError != "" {
		t.Errorf("row = %+v, want DONE and no longer failed", r)
	}
	if _, ok, _ := s.UnfinishedUploadBySource(ctx, "card/IMG_1.CR3", rec.FullHash); ok {
		t.Error("a DONE row still reads as unfinished")
	}
}
