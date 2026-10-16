package ingest

import (
	"context"
	"errors"
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

// TestDrainRetriesLeftoverArchiveCopyOnceBeforeFailing: a post-rename
// read-back that ERRORS leaves the placed copy in place on purpose. When a
// later pass's existing-file check then reads that leftover back wrong, it
// is (most likely) the agent's own copy, not "a different file": the row
// gets the same one grace retry as a fresh copy that reads back wrong, and
// fails on the second bad read. The leftover is never removed or changed
// by the existing-file check, since it cannot be proven to be the agent's.
func TestDrainRetriesLeftoverArchiveCopyOnceBeforeFailing(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	content := []byte("as-ingested")
	seedMediaRow(t, store, dir, "uuid-leftover", content)
	archive := filepath.Join(dir, "archive", "uuid-leftover.bin")
	ctx := context.Background()

	orig := archiveVerifyFn
	t.Cleanup(func() { archiveVerifyFn = orig })
	readErr := func(string, string, ...WriteOption) (VerifyResult, error) {
		return VerifyResult{}, errors.New("simulated NAS read failure")
	}
	badRead := func(path, _ string, _ ...WriteOption) (VerifyResult, error) {
		return VerifyResult{Path: path, Method: VerifyMethodUnbuffered, Hash: "garbled", Verified: false}, nil
	}
	leftoverIntact := func(pass int) {
		t.Helper()
		if got, err := os.ReadFile(archive); err != nil || string(got) != string(content) {
			t.Fatalf("pass %d: leftover archive copy removed or changed: %q, %v", pass, got, err)
		}
	}

	// Pass 1: the copy is placed, its read-back errors -- left in place,
	// transient.
	archiveVerifyFn = readErr
	t0 := time.Unix(1_800_000_000, 0)
	stats, err := Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(t0))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.ByNodeUUID(ctx, "uuid-leftover")
	if stats.ArchiveCopiesFailed != 0 || rec.ArchiveCopyStatus != queue.StatusPending {
		t.Fatalf("pass 1: failed=%d status=%s, want PENDING after a read error", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus)
	}
	leftoverIntact(1)

	// Pass 2: the existing-file check reads the leftover back wrong once:
	// a read-back mismatch (retried), not a conflict (terminal at once).
	archiveVerifyFn = badRead
	stats, err = Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(t0.Add(backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.ByNodeUUID(ctx, "uuid-leftover")
	if stats.ArchiveCopiesFailed != 0 || rec.ArchiveCopyStatus != queue.StatusPending {
		t.Fatalf("pass 2: failed=%d status=%s (%q), want PENDING after one bad read of the leftover", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus, rec.ArchiveCopyLastError)
	}
	if !strings.Contains(rec.ArchiveCopyLastError, ErrArchiveVerifyMismatch.Error()) || strings.Contains(rec.ArchiveCopyLastError, ErrArchiveConflict.Error()) {
		t.Errorf("pass 2: last error %q, want a read-back mismatch, not a conflict", rec.ArchiveCopyLastError)
	}
	leftoverIntact(2)

	// Pass 3: a second bad read in a row fails the row.
	stats, err = Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(t0.Add(2*backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ = store.ByNodeUUID(ctx, "uuid-leftover")
	if stats.ArchiveCopiesFailed != 1 || rec.ArchiveCopyStatus != queue.StatusFailed {
		t.Fatalf("pass 3: failed=%d status=%s, want FAILED after a second bad read", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus)
	}
	leftoverIntact(3)
}

// TestCopyToArchiveExistingFileClassifiedBySize: a non-matching file at the
// archive path is a read-back mismatch only when it has exactly the local
// copy's size (the shape of the agent's own leftover); any other size is a
// conflict, as before. Neither case touches the existing file.
func TestCopyToArchiveExistingFileClassifiedBySize(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
		want     error
	}{
		{"same size", "XXXXXXXXXXXXX", ErrArchiveVerifyMismatch},
		{"different size", "someone else's verified master", ErrArchiveConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			localPath := filepath.Join(dir, "local.bin")
			if err := os.WriteFile(localPath, []byte("payload-bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(dir, "archive", "final.bin")
			if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archivePath, []byte(tc.existing), 0o644); err != nil {
				t.Fatal(err)
			}

			err := CopyToArchive(localPath, archivePath, blake3Hex(t, localPath))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got, rerr := os.ReadFile(archivePath); rerr != nil || string(got) != tc.existing {
				t.Errorf("existing archive file removed or changed: %q, %v", got, rerr)
			}
			if _, serr := os.Stat(tempArchiveName(archivePath)); !os.IsNotExist(serr) {
				t.Errorf("archive temp file created for an occupied destination: %v", serr)
			}
		})
	}
}

// TestDrainResetRowGetsItsGraceRetryAgain: ResetFailed zeroes the attempt
// counter but keeps archive_copy_last_error, so the stale mismatch text
// alone must not fail the row on the first mismatch after a reset.
func TestDrainResetRowGetsItsGraceRetryAgain(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-reset", []byte("as-ingested"))

	orig := archiveVerifyFn
	archiveVerifyFn = func(path, _ string, _ ...WriteOption) (VerifyResult, error) {
		return VerifyResult{Path: path, Method: VerifyMethodUnbuffered, Hash: "garbled", Verified: false}, nil
	}
	t.Cleanup(func() { archiveVerifyFn = orig })

	ctx := context.Background()
	t0 := time.Unix(1_800_000_000, 0)
	for i, at := range []time.Time{t0, t0.Add(backoffCap)} {
		if _, err := Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(at)); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if rec, _, _ := store.ByNodeUUID(ctx, "uuid-reset"); rec.ArchiveCopyStatus != queue.StatusFailed {
		t.Fatalf("setup: status=%s after two mismatches, want FAILED", rec.ArchiveCopyStatus)
	}

	if _, err := store.ResetFailed(ctx); err != nil {
		t.Fatal(err)
	}
	stats, err := Drain(ctx, &fakeDrainClient{}, store, "agent-1", fixedNow(t0.Add(2*backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := store.ByNodeUUID(ctx, "uuid-reset")
	if stats.ArchiveCopiesFailed != 0 || rec.ArchiveCopyStatus != queue.StatusPending {
		t.Fatalf("first mismatch after reset: failed=%d status=%s, want PENDING (one grace retry)", stats.ArchiveCopiesFailed, rec.ArchiveCopyStatus)
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
