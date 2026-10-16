package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// recordDirSyncs replaces syncParentDirFn with a recorder for the test.
func recordDirSyncs(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var dirs []string
	orig := syncParentDirFn
	syncParentDirFn = func(dir string) error {
		mu.Lock()
		dirs = append(dirs, dir)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { syncParentDirFn = orig })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dirs...)
	}
}

// TestCopyToArchivePreservesMtimeAndSyncsDir: the archive master gets the
// local copy's (that is, the card file's) mtime, like an online DualWrite
// master, and its directory entry is fsynced after the rename.
func TestCopyToArchivePreservesMtimeAndSyncsDir(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "local", "IMG_0001.CR3")
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("raw-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2024, 5, 17, 9, 30, 0, 0, time.UTC)
	if err := os.Chtimes(local, captured, captured); err != nil {
		t.Fatal(err)
	}
	synced := recordDirSyncs(t)

	archive := filepath.Join(dir, "archive", "2024", "IMG_0001.CR3")
	if err := CopyToArchive(local, archive, blake3Hex(t, local)); err != nil {
		t.Fatalf("CopyToArchive: %v", err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(captured) {
		t.Errorf("archive mtime = %v, want the local copy's %v", info.ModTime(), captured)
	}
	found := false
	for _, d := range synced() {
		if d == filepath.Dir(archive) {
			found = true
		}
	}
	if !found {
		t.Errorf("dir fsyncs = %v, want %s after the rename", synced(), filepath.Dir(archive))
	}
}

// TestOfflineLocalPlacementSyncsDir: the offline local copy's rename into
// place is followed by a dir fsync too (renameNoClobber).
func TestOfflineLocalPlacementSyncsDir(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".x.branchdam-agent-tmp")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	synced := recordDirSyncs(t)
	if err := renameNoClobber(tmp, filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	if got := synced(); len(got) != 1 || got[0] != dir {
		t.Errorf("dir fsyncs = %v, want [%s]", got, dir)
	}
}

// TestDrainFailsRowOnRepeatedArchiveReadBackMismatch: a placed copy that
// reads back with the wrong hash is retried once, then the row fails --
// instead of being copied to the archive again every backoff interval
// forever.
func TestDrainFailsRowOnRepeatedArchiveReadBackMismatch(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-readback", []byte("as-ingested"))
	archive := filepath.Join(dir, "archive", "uuid-readback.bin")

	orig := archiveVerifyFn
	archiveVerifyFn = func(path, _ string, _ ...WriteOption) (VerifyResult, error) {
		return VerifyResult{Path: path, Method: VerifyMethodUnbuffered, Hash: "garbled", Verified: false}, nil
	}
	t.Cleanup(func() { archiveVerifyFn = orig })

	t0 := time.Unix(1_800_000_000, 0)
	stats, err := Drain(context.Background(), &fakeDrainClient{}, store, "agent-1", fixedNow(t0))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.ByNodeUUID(context.Background(), "uuid-readback")
	if stats.ArchiveCopiesFailed != 0 || rec.ArchiveCopyStatus != queue.StatusPending {
		t.Fatalf("after one mismatch: failed=%d status=%s, want one retry allowed", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus)
	}
	if !strings.Contains(rec.ArchiveCopyLastError, ErrArchiveVerifyMismatch.Error()) {
		t.Errorf("last error %q does not record the read-back mismatch", rec.ArchiveCopyLastError)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Errorf("mismatching placed copy left at %s: %v", archive, err)
	}

	stats, err = Drain(context.Background(), &fakeDrainClient{}, store, "agent-1", fixedNow(t0.Add(backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.ByNodeUUID(context.Background(), "uuid-readback")
	if stats.ArchiveCopiesFailed != 1 || rec.ArchiveCopyStatus != queue.StatusFailed {
		t.Fatalf("after a second mismatch: failed=%d status=%s, want FAILED", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus)
	}
	if pending, _ := store.Pending(context.Background()); len(pending) != 0 {
		t.Errorf("Pending() = %d rows, want the failed row out of the work list", len(pending))
	}
}

// TestDrainEditedLocalCopyStaysTerminal: once an edited local copy has
// failed its row, later passes never pick the row up again -- even after
// the backoff cap and even if the file is changed back.
func TestDrainEditedLocalCopyStaysTerminal(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-edit", []byte("as-ingested"))
	local := filepath.Join(dir, "local", "uuid-edit.bin")
	if err := os.WriteFile(local, []byte("edited-in-place"), 0o644); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_800_000_000, 0)
	if _, err := Drain(context.Background(), &fakeDrainClient{}, store, "agent-1", fixedNow(t0)); err != nil {
		t.Fatal(err)
	}
	before, _, _ := store.ByNodeUUID(context.Background(), "uuid-edit")
	if before.ArchiveCopyStatus != queue.StatusFailed {
		t.Fatalf("status = %s, want FAILED", before.ArchiveCopyStatus)
	}

	if err := os.WriteFile(local, []byte("as-ingested"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := Drain(context.Background(), &fakeDrainClient{}, store, "agent-1", fixedNow(t0.Add(time.Duration(i)*backoffCap))); err != nil {
			t.Fatal(err)
		}
	}
	after, _, _ := store.ByNodeUUID(context.Background(), "uuid-edit")
	if after.ArchiveCopyStatus != queue.StatusFailed || after.ArchiveCopyAttempts != before.ArchiveCopyAttempts {
		t.Errorf("row touched again: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(dir, "archive", "uuid-edit.bin")); !os.IsNotExist(err) {
		t.Errorf("a FAILED row was copied to the archive by a later pass: %v", err)
	}
}

// TestDrainCollectsOldFinishedRows: Drain runs the throttled row GC; a
// finished row past the retention whose local copy is gone is deleted.
func TestDrainCollectsOldFinishedRows(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	ctx := context.Background()
	seedMediaRow(t, store, dir, "uuid-old", []byte("old"))
	_ = store.MarkNodeCreatedSubmitted(ctx, "uuid-old", "evt", time.Now())
	_ = store.MarkArchiveCopyDone(ctx, "uuid-old")
	_ = store.MarkRebaseDone(ctx, "uuid-old")
	if err := os.Remove(filepath.Join(dir, "local", "uuid-old.bin")); err != nil { // pruned long ago
		t.Fatal(err)
	}

	stats, err := Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(time.Now().Add(queue.GCRetention+time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if stats.RowsCollected != 1 {
		t.Errorf("RowsCollected = %d, want 1", stats.RowsCollected)
	}
	if all, _ := store.All(ctx); len(all) != 0 {
		t.Errorf("%d rows left, want 0", len(all))
	}
}
