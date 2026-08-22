package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// blake3Hex computes the same BLAKE3-256 hex digest DualWrite/WriteLocal
// produce, directly from an on-disk file -- used by tests that need
// CopyToArchive's wantFullHash without threading a WriteResult through.
func blake3Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := blake3.New()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func newOfflineTestEngine(t *testing.T, client nodeCreator, archiveRoot, localRoot, tier0Root string, store *queue.Store) *Engine {
	t.Helper()
	e := newTestEngine(t, client, archiveRoot, localRoot)
	e.Queue = store
	e.Tier0ContainerRoot = tier0Root
	return e
}

func openStoreT(t *testing.T) *queue.Store {
	t.Helper()
	s, err := queue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// failingClient always fails PostNodeCreated -- simulates "genuinely
// offline, no route to the server at all" for the opportunistic inline
// attempt.
type failingClient struct{}

func (failingClient) PostNodeCreated(context.Context, string, branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
	return nil, context.DeadlineExceeded
}

func TestIngestCardOfflineQueuesRowBeforeSubmission(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("file error: %v", fr.Err)
	}
	if !fr.Queued {
		t.Fatal("expected Queued=true")
	}
	if fr.SubmittedInline {
		t.Error("expected SubmittedInline=false: client always fails")
	}
	if !fr.LocalVerify.Verified {
		t.Error("expected local copy to verify")
	}

	// Local copy landed on disk.
	if _, err := os.Stat(fr.LocalPath); err != nil {
		t.Errorf("local copy missing: %v", err)
	}
	// Archive copy was never attempted in the offline path.
	archivePath := filepath.Join(dir, "archive", "IMG_0001.jpg")
	if _, err := os.Stat(archivePath); err == nil {
		t.Error("expected no archive copy to exist yet")
	}

	rec, ok, err := store.ByNodeUUID(context.Background(), fr.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("expected a queue row for %s: ok=%v err=%v", fr.NodeUUID, ok, err)
	}
	if rec.NodeCreatedStatus != queue.StatusPending {
		t.Errorf("NodeCreatedStatus = %q, want PENDING (submission failed)", rec.NodeCreatedStatus)
	}
	if rec.ArchiveCopyStatus != queue.StatusPending {
		t.Errorf("ArchiveCopyStatus = %q, want PENDING", rec.ArchiveCopyStatus)
	}
	if rec.RebaseStatus != queue.StatusPending {
		t.Errorf("RebaseStatus = %q, want PENDING", rec.RebaseStatus)
	}
	if rec.Tier0ContainerPath != "/storage/staging/agent-1/IMG_0001.jpg" {
		t.Errorf("Tier0ContainerPath = %q", rec.Tier0ContainerPath)
	}
	if rec.ArchiveContainerPath != "/storage/archive/IMG_0001.jpg" {
		t.Errorf("ArchiveContainerPath = %q", rec.ArchiveContainerPath)
	}

	var payload branchdam.NodeCreatedPayload
	if err := json.Unmarshal([]byte(rec.NodeCreatedPayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FilePath != rec.Tier0ContainerPath {
		t.Errorf("stored payload FilePath = %q, want %q (Tier-0, not final archive path)", payload.FilePath, rec.Tier0ContainerPath)
	}
}

func TestIngestCardOfflineOpportunisticSubmitSucceeds(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	client := &fakeClient{}
	e := newOfflineTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if !fr.SubmittedInline {
		t.Error("expected SubmittedInline=true: fakeClient always succeeds")
	}

	rec, ok, err := store.ByNodeUUID(context.Background(), fr.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.NodeCreatedStatus != queue.StatusSubmitted {
		t.Errorf("NodeCreatedStatus = %q, want SUBMITTED", rec.NodeCreatedStatus)
	}
	if rec.NodeCreatedEventID == "" {
		t.Error("expected a stored event ID")
	}
}

func TestIngestCardOfflineSidecarNeverSubmitsEvent(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "a.xmp"), []byte("<xmp/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	client := &fakeClient{}
	e := newOfflineTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if !fr.Skipped {
		t.Error("expected sidecar to be Skipped")
	}
	if len(client.calls) != 0 {
		t.Errorf("expected no PostNodeCreated calls for a sidecar, got %d", len(client.calls))
	}

	rec, ok, err := store.ByNodeUUID(context.Background(), fr.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.Kind != queue.KindSidecar {
		t.Errorf("Kind = %q, want SIDECAR", rec.Kind)
	}
	if rec.NodeCreatedStatus != queue.StatusSkipped || rec.RebaseStatus != queue.StatusSkipped {
		t.Errorf("expected node_created/rebase SKIPPED for a sidecar, got %+v", rec)
	}
	if rec.ArchiveCopyStatus != queue.StatusPending {
		t.Errorf("ArchiveCopyStatus = %q, want PENDING (still needs to land in the archive)", rec.ArchiveCopyStatus)
	}
}

// TestIngestCardOfflineResumeAfterRestart is the in-process half of the
// crash-safety property: a second Engine (simulating a fresh process after
// restart, sharing only the same on-disk queue.db and local copy -- no
// shared Go state) ingesting the SAME card must not re-copy the local file,
// must not mint a new NodeUUID, and must not produce a second queue row.
// The out-of-process version (a real killed subprocess) lives in
// cmd/branchdam-agent's offline_crash_test.go.
func TestIngestCardOfflineResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	queueDBPath := filepath.Join(dir, "queue.db")
	store1, err := queue.Open(queueDBPath)
	if err != nil {
		t.Fatal(err)
	}

	e1 := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store1)
	res1, err := e1.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	firstUUID := res1.Files[0].NodeUUID
	if firstUUID == "" {
		t.Fatal("expected a minted NodeUUID")
	}
	// Simulate a crash: no Close(), no cleanup -- just stop using store1 and
	// open a fresh Store handle over the same file, as a restarted process
	// would.
	_ = store1.Close()

	store2, err := queue.Open(queueDBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store2.Close() }()

	// A NewNodeUUID that panics if called proves the resume path never
	// mints a fresh UUID for an already-queued source file.
	e2 := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store2)
	e2.NewNodeUUID = func() (string, error) {
		t.Fatal("NewNodeUUID must not be called when resuming an already-queued file")
		return "", nil
	}

	res2, err := e2.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr2 := res2.Files[0]
	if !fr2.AlreadyQueued {
		t.Error("expected AlreadyQueued=true on resume")
	}
	if fr2.NodeUUID != firstUUID {
		t.Errorf("resumed NodeUUID = %q, want %q (the original, never re-minted)", fr2.NodeUUID, firstUUID)
	}

	all, err := store2.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d queue rows after resume, want exactly 1 (no duplicate)", len(all))
	}
}

// TestIngestCardOfflineCleansUpOrphanedPartial simulates a crash between
// WriteLocal succeeding and InsertPending committing: a local file exists
// with no queue row referencing it. A restart must not treat that as
// "already done" -- it must clean up and redo the local write, since
// nothing was ever durably promised for that file.
func TestIngestCardOfflineCleansUpOrphanedPartial(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(cardRoot, "a.jpg")
	if err := os.WriteFile(srcPath, []byte("real-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	localRoot := filepath.Join(dir, "local")
	// Pre-create a bogus partial local file at the destination the engine
	// will compute, with no matching queue row -- exactly what WriteLocal
	// would leave behind if the process died before InsertPending.
	orphanPath := filepath.Join(localRoot, "a.jpg")
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("garbage-partial-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), localRoot, "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("expected the orphan to be cleaned up and the file re-ingested, got error: %v", fr.Err)
	}
	if !fr.LocalVerify.Verified {
		t.Error("expected the freshly-rewritten local copy to verify")
	}

	got, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "real-data" {
		t.Errorf("local copy = %q, want the real source content, not the orphaned garbage", got)
	}
}

func TestCopyToArchiveResumesAfterPartialTemp(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "local.bin")
	if err := os.WriteFile(localPath, []byte("payload-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "archive", "final.bin")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Leave a stale temp file behind, as a killed CopyToArchive would.
	tmp := tempArchiveName(archivePath)
	if err := os.WriteFile(tmp, []byte("stale-incomplete-garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	wantHash := blake3Hex(t, localPath)

	if err := CopyToArchive(localPath, archivePath, wantHash); err != nil {
		t.Fatalf("CopyToArchive: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("expected the stale temp file to be gone")
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload-bytes" {
		t.Errorf("archive content = %q", got)
	}
}

func TestCopyToArchiveIsIdempotentOnAlreadyVerifiedFile(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "local.bin")
	if err := os.WriteFile(localPath, []byte("payload-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, "archive", "final.bin")

	wantHash := blake3Hex(t, localPath)

	if err := CopyToArchive(localPath, archivePath, wantHash); err != nil {
		t.Fatalf("first CopyToArchive: %v", err)
	}
	modBefore, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// A second call (simulating a drain pass that re-attempts a row whose
	// status update didn't get recorded before a crash) must recognize the
	// existing, verified file and succeed without rewriting it.
	if err := CopyToArchive(localPath, archivePath, wantHash); err != nil {
		t.Fatalf("second CopyToArchive: %v", err)
	}
	modAfter, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !modBefore.ModTime().Equal(modAfter.ModTime()) {
		t.Error("expected the second call to leave the already-verified file untouched")
	}
}
