package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func (failingClient) PostNodeCreatedWithUUID(context.Context, string, branchdam.NodeCreatedPayload, string) (*branchdam.EventResponse, error) {
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
	if len(client.uuidCalls) != 1 {
		t.Fatalf("expected 1 PostNodeCreatedWithUUID call, got %d", len(client.uuidCalls))
	}
	if client.uuidCalls[0] == "" {
		t.Error("expected non-empty eventUUID on opportunistic submission")
	}
	if rec.NodeCreatedEventUUID != client.uuidCalls[0] {
		t.Errorf("queue rec.NodeCreatedEventUUID = %q, want submitted %q", rec.NodeCreatedEventUUID, client.uuidCalls[0])
	}
}

// TestIngestCardOfflineEventUUIDStabilityInlineToDrain tests that the same
// client-minted EventUUID is preserved end-to-end between an opportunistic
// inline submission failure and the subsequent offline queue drain re-send.
func TestIngestCardOfflineEventUUIDStabilityInlineToDrain(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "shot.jpg"), []byte("image-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)

	shouldFail := true
	client := &fakeDrainClient{
		nodeCreated: func(p branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
			if shouldFail {
				return nil, fmt.Errorf("simulated network blip on opportunistic inline submit")
			}
			return &branchdam.EventResponse{EventID: "evt-offline-success"}, nil
		},
	}

	e := newOfflineTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if !fr.Queued {
		t.Fatal("expected Queued=true")
	}
	if fr.SubmittedInline {
		t.Fatal("expected SubmittedInline=false due to simulated network blip")
	}

	if len(client.nodeCreatedUUIDCalls) != 1 {
		t.Fatalf("expected 1 inline opportunistic call, got %d", len(client.nodeCreatedUUIDCalls))
	}
	inlineUUID := client.nodeCreatedUUIDCalls[0]
	if inlineUUID == "" {
		t.Fatal("expected non-empty eventUUID on inline opportunistic attempt")
	}

	rec, ok, err := store.ByNodeUUID(context.Background(), fr.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.NodeCreatedEventUUID != inlineUUID {
		t.Errorf("stored NodeCreatedEventUUID = %q, want inline attempt %q", rec.NodeCreatedEventUUID, inlineUUID)
	}

	// Now network recovers; Drain the offline queue.
	shouldFail = false
	if _, err := Drain(context.Background(), client, store, "agent-1", e.Now); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(client.nodeCreatedUUIDCalls) != 2 {
		t.Fatalf("expected 2 calls total (1 inline + 1 drain), got %d", len(client.nodeCreatedUUIDCalls))
	}
	drainUUID := client.nodeCreatedUUIDCalls[1]

	// Assert the UUID sent during drain matches the one sent during the failed inline attempt
	if drainUUID != inlineUUID {
		t.Errorf("drain retry eventUUID = %q, want inline eventUUID %q", drainUUID, inlineUUID)
	}

	recAfter, ok, err := store.ByNodeUUID(context.Background(), fr.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if recAfter.NodeCreatedStatus != queue.StatusSubmitted {
		t.Errorf("NodeCreatedStatus after drain = %q, want SUBMITTED", recAfter.NodeCreatedStatus)
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

// TestIngestCardOfflineNeverDeletesUnclaimedLocalFile is the regression
// test for offline ingest deleting a legitimate local edit copy: a file at
// the rendered destination with no queue row claiming it used to be
// treated as this agent's own "orphaned partial" and os.Remove'd. Online
// ingest never writes queue rows, so that was every online-ingested edit
// copy (and anything the user put there). It must survive untouched and
// the new file must take the next suffix.
func TestIngestCardOfflineNeverDeletesUnclaimedLocalFile(t *testing.T) {
	cases := []struct {
		name     string
		existing string
	}{
		{"different content", "someone-elses-edit-copy"},
		// Identical but unclaimed: proves nothing about the archive, so it
		// is not trusted as "already ingested" either.
		{"identical content", "real-data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cardRoot := filepath.Join(dir, "card")
			if err := os.MkdirAll(cardRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("real-data"), 0o644); err != nil {
				t.Fatal(err)
			}

			localRoot := filepath.Join(dir, "local")
			existingPath := filepath.Join(localRoot, "a.jpg")
			if err := os.MkdirAll(localRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(existingPath, []byte(tc.existing), 0o644); err != nil {
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
				t.Fatalf("unexpected error: %v", fr.Err)
			}
			if got := fr.Outcome(); got != OfflineOutcomeQueued {
				t.Fatalf("Outcome() = %v, want OfflineOutcomeQueued (skip reason %q)", got, fr.SkipReason)
			}

			got, err := os.ReadFile(existingPath)
			if err != nil {
				t.Fatalf("the pre-existing local file was removed: %v", err)
			}
			if string(got) != tc.existing {
				t.Errorf("pre-existing local file = %q, want it untouched (%q)", got, tc.existing)
			}

			wantNew := filepath.Join(localRoot, "a_2.jpg")
			if fr.LocalPath != wantNew {
				t.Errorf("new file's LocalPath = %s, want the suffixed %s", fr.LocalPath, wantNew)
			}
			if got, err := os.ReadFile(wantNew); err != nil || string(got) != "real-data" {
				t.Errorf("new local copy = %q, %v; want the source content", got, err)
			}
			if _, err := os.Stat(localTempName(wantNew)); !os.IsNotExist(err) {
				t.Errorf("temp copy left behind after a successful ingest: %v", err)
			}
		})
	}
}

// TestIngestCardOfflineReplacesCrashLeftTemp: a run that died after writing
// its temp copy but before inserting the queue row leaves only the agent's
// own temp name behind -- never a file at the final name. The next run
// overwrites that temp and completes normally.
func TestIngestCardOfflineReplacesCrashLeftTemp(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("real-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Join(dir, "local")
	final := filepath.Join(localRoot, "a.jpg")
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localTempName(final), []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), localRoot, "/storage/staging/agent-1", store)
	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	fr := res.Files[0]
	if fr.Err != nil || fr.LocalPath != final {
		t.Fatalf("got LocalPath=%s err=%v, want %s and no error", fr.LocalPath, fr.Err, final)
	}
	if got, _ := os.ReadFile(final); string(got) != "real-data" {
		t.Errorf("local copy = %q, want the source content", got)
	}
	if _, err := os.Stat(localTempName(final)); !os.IsNotExist(err) {
		t.Errorf("crash-left temp still present: %v", err)
	}
}

// TestIngestCardOfflineResumeFinishesUnplacedLocalCopy covers a run that
// died after inserting the queue row but before renaming its temp copy
// into place: the row claims a local path nothing sits at. Re-ingesting the
// card resumes the row (same NodeUUID, no new row) and puts the local copy
// in place -- from the verified temp when it survived, from the card when
// it didn't -- so Drain has something to archive.
func TestIngestCardOfflineResumeFinishesUnplacedLocalCopy(t *testing.T) {
	cases := []struct {
		name string
		// prepare mutates the on-disk state after the first ingest to
		// simulate the crash.
		prepare func(t *testing.T, final string)
	}{
		{"temp survived", func(t *testing.T, final string) {
			if err := os.Rename(final, localTempName(final)); err != nil {
				t.Fatal(err)
			}
		}},
		{"temp truncated", func(t *testing.T, final string) {
			if err := os.Remove(final); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localTempName(final), []byte("trunc"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"nothing on disk", func(t *testing.T, final string) {
			if err := os.Remove(final); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cardRoot := filepath.Join(dir, "card")
			if err := os.MkdirAll(cardRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("real-data"), 0o644); err != nil {
				t.Fatal(err)
			}
			store := openStoreT(t)
			e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
			res1, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil || res1.Files[0].Err != nil {
				t.Fatalf("first ingest: %v / %v", err, res1.Files[0].Err)
			}
			final := res1.Files[0].LocalPath
			tc.prepare(t, final)

			e.NewNodeUUID = func() (string, error) {
				t.Fatal("NewNodeUUID must not be called on resume")
				return "", nil
			}
			res2, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil {
				t.Fatal(err)
			}
			fr := res2.Files[0]
			if got := fr.Outcome(); got != OfflineOutcomeResumed {
				t.Fatalf("Outcome() = %v, want resumed (err=%v)", got, fr.Err)
			}
			if got, err := os.ReadFile(final); err != nil || string(got) != "real-data" {
				t.Errorf("local copy after resume = %q, %v; want the source content in place", got, err)
			}
			if _, err := os.Stat(localTempName(final)); !os.IsNotExist(err) {
				t.Errorf("temp copy left behind after resume: %v", err)
			}
			all, _ := store.All(context.Background())
			if len(all) != 1 {
				t.Errorf("got %d rows, want 1", len(all))
			}
		})
	}
}

// TestIngestCardOfflineAvoidsReachableArchiveCollision: when the archive is
// reachable at offline-ingest time, a name another file already holds in
// the archive is not handed to the new file (which would only make the
// drain fail the row later); an identical archive file is fine to share.
func TestIngestCardOfflineAvoidsReachableArchiveCollision(t *testing.T) {
	cases := []struct {
		name        string
		archived    string
		reachable   bool
		wantLocal   string
		wantArchive string
	}{
		{"different master in reachable archive", "another-cards-master", true, "a_2.jpg", "a_2.jpg"},
		{"identical master in reachable archive", "real-data", true, "a.jpg", "a.jpg"},
		{"archive unreachable", "another-cards-master", false, "a.jpg", "a.jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cardRoot := filepath.Join(dir, "card")
			if err := os.MkdirAll(cardRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cardRoot, "a.jpg"), []byte("real-data"), 0o644); err != nil {
				t.Fatal(err)
			}
			archiveRoot := filepath.Join(dir, "archive")
			if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			master := filepath.Join(archiveRoot, "a.jpg")
			if err := os.WriteFile(master, []byte(tc.archived), 0o644); err != nil {
				t.Fatal(err)
			}

			store := openStoreT(t)
			e := newOfflineTestEngine(t, failingClient{}, archiveRoot, filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
			e.ArchiveReachable = func(context.Context, string) bool { return tc.reachable }

			res, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil {
				t.Fatal(err)
			}
			fr := res.Files[0]
			if fr.Err != nil || !fr.Queued {
				t.Fatalf("got %+v, want a queued file", fr)
			}
			if want := filepath.Join(dir, "local", tc.wantLocal); fr.LocalPath != want {
				t.Errorf("LocalPath = %s, want %s", fr.LocalPath, want)
			}
			rec, _, _ := store.ByNodeUUID(context.Background(), fr.NodeUUID)
			if want := filepath.Join(archiveRoot, tc.wantArchive); rec.ArchivePath != want {
				t.Errorf("ArchivePath = %s, want %s", rec.ArchivePath, want)
			}
			if got, _ := os.ReadFile(master); string(got) != tc.archived {
				t.Errorf("existing archive file = %q, want it untouched", got)
			}
		})
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

// TestIngestCardOfflineSkipIdenticalDuplicate mirrors
// TestIngestCardReingestSkipIdentical for the offline path: two distinct
// source files (different folders, identical basename and content) collide
// on the same rendered destination. ResolveDestination reports
// AlreadyIngested for the second one, which must be honored -- skipped
// without deleting the first file's already-verified local copy, minting a
// second NodeUUID, or inserting a second queue row for the same content.
func TestIngestCardOfflineSkipIdenticalDuplicate(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	dir1 := filepath.Join(cardRoot, "DCIM", "100MSDCF")
	dir2 := filepath.Join(cardRoot, "DCIM", "101MSDCF")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "DSC0001.JPG"), []byte("identical-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "DSC0001.JPG"), []byte("identical-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
	e.Ingest.PathTemplate = "{original_name}"

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(res.Files))
	}

	var queued, skipped int
	for _, f := range res.Files {
		if f.Err != nil {
			t.Fatalf("unexpected error for %s: %v", f.SourcePath, f.Err)
		}
		if f.Skipped {
			skipped++
			if f.SkipReason != "already ingested (identical file exists at destination)" {
				t.Errorf("got skip reason %q", f.SkipReason)
			}
		} else if f.Queued {
			queued++
		}
	}
	if queued != 1 || skipped != 1 {
		t.Fatalf("got queued=%d skipped=%d, want 1 and 1", queued, skipped)
	}

	// Only the first file's content should remain -- the second run must
	// not have deleted and rewritten it out from under the first NodeUUID.
	localPath := filepath.Join(dir, "local", "DSC0001.JPG")
	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("local copy missing: %v", err)
	}
	if string(data) != "identical-content" {
		t.Errorf("local copy content = %q", data)
	}

	// No suffixed sibling should have been created -- the duplicate was
	// skipped, not auto-suffixed as a distinct file.
	if _, err := os.Stat(filepath.Join(dir, "local", "DSC0001_2.JPG")); err == nil {
		t.Error("did not expect a suffixed DSC0001_2.JPG -- duplicate should have been skipped, not renamed")
	}
}

// TestIngestCardOfflineSkipsOSMetadata is the offline mirror of
// TestIngestCardSkipsOSMetadata: a macOS- or Windows-formatted card's
// per-directory junk files must not become queue.db rows, must not
// trigger exiftool, and must not write to the local edit destination.
func TestIngestCardOfflineSkipsOSMetadata(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	subdir := filepath.Join(cardRoot, "DCIM", "100MSDCF")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "DSC0001.JPG"), []byte("photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(cardRoot, ".DS_Store"),
		filepath.Join(cardRoot, "._DSC0001.JPG"),
		filepath.Join(subdir, "Thumbs.db"),
	} {
		if err := os.WriteFile(p, []byte("os-junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v", err)
	}
	if len(res.Files) != 4 {
		t.Fatalf("got %d files, want 4 (1 photo + 3 OS-metadata)", len(res.Files))
	}

	// Exactly one real photo gets queued; the 3 OS-metadata files are
	// Skipped, have no NodeUUID, and are not in queue.db.
	queued, junk := 0, 0
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Fatalf("unexpected error for %s: %v", fr.SourcePath, fr.Err)
		}
		if fr.Skipped {
			junk++
			if fr.NodeUUID != "" {
				t.Errorf("OS-metadata file %s was Skipped but has a NodeUUID (%s) -- it must not have been queued", filepath.Base(fr.SourcePath), fr.NodeUUID)
			}
			if fr.Queued {
				t.Errorf("OS-metadata file %s marked Queued -- must not reach queue.db", filepath.Base(fr.SourcePath))
			}
		} else {
			queued++
		}
	}
	if queued != 1 || junk != 3 {
		t.Errorf("got queued=%d junk=%d, want 1 and 3", queued, junk)
	}

	// queue.db must contain exactly the one real photo's row.
	all, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("got %d queue rows, want 1 (only the real photo)", len(all))
	}

	// Local edit copy must not contain any of the OS-metadata files.
	localRoot := filepath.Join(dir, "local")
	for _, name := range []string{".DS_Store", "Thumbs.db", "._DSC0001.JPG"} {
		if _, err := os.Stat(filepath.Join(localRoot, name)); err == nil {
			t.Errorf("OS-metadata file %s leaked into local destination", name)
		}
	}
}

// TestIngestCardOfflineAllowedExtensionsFilter pins the offline path's
// behavior when Ingest.AllowedExtensions is set: only matching
// extensions are queued; rejected ones are reported as Skipped and do
// not become queue.db rows.
func TestIngestCardOfflineAllowedExtensionsFilter(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jpg", "b.txt", "c.mp4"} {
		if err := os.WriteFile(filepath.Join(cardRoot, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
	e.Ingest.AllowedExtensions = []string{"jpg", "mp4"}
	// newTestEngine's NewNodeUUID is a fixed string; the queue's
	// node_uuid UNIQUE constraint would reject every file past the
	// first. Use a counter that returns a unique value per call.
	var uuidCounter int
	e.NewNodeUUID = func() (string, error) {
		uuidCounter++
		return fmt.Sprintf("0000000-uuid-%d", uuidCounter), nil
	}

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v", err)
	}
	if len(res.Files) != 3 {
		t.Fatalf("got %d files, want 3", len(res.Files))
	}
	queued, filtered := 0, 0
	for _, fr := range res.Files {
		base := filepath.Base(fr.SourcePath)
		switch base {
		case "a.jpg", "c.mp4":
			if fr.Skipped {
				t.Errorf("%s must NOT be skipped (matches allowlist)", base)
			}
			if !fr.Queued {
				t.Errorf("%s must be queued", base)
			}
			queued++
		default:
			if !fr.Skipped {
				t.Errorf("%s must be skipped (not in allowlist)", base)
			}
			if fr.Queued {
				t.Errorf("%s Skipped but marked Queued", base)
			}
			filtered++
		}
	}
	if queued != 2 || filtered != 1 {
		t.Errorf("got queued=%d filtered=%d, want 2 and 1", queued, filtered)
	}

	all, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("got %d queue rows, want 2 (jpg+mp4 only)", len(all))
	}
}

// TestIngestCardOfflineChtimesFailureIsLogged is the offline twin of
// TestIngestCardChtimesFailureIsLogged (issue #103): the offline path
// has exactly one os.Chtimes call (on the local edit copy, because the
// archive copy is deferred to Drain). Same soft contract -- failure
// logs, doesn't fail -- but here the local-path mtime IS the one the
// prune-safety half of invariant #8 reads back (see internal/prune/
// prune.go's TOCTOU re-stat), so the warn is the operator's only signal
// that a queue row's MtimeUnix may diverge from the actual file's mtime.
func TestIngestCardOfflineChtimesFailureIsLogged(t *testing.T) {
	origChtimes := cHtimesFn
	t.Cleanup(func() { cHtimesFn = origChtimes })
	chtimesErr := errors.New("simulated chtimes failure")
	cHtimesFn = func(path string, atime, mtime time.Time) error {
		return &os.PathError{Op: "chtimes", Path: path, Err: chtimesErr}
	}

	logBuf := captureSlog(t)

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

	// Soft contract: the local file is on disk, hashes verified, queue row
	// inserted -- a Chtimes failure must not abort any of that.
	if fr.Err != nil {
		t.Fatalf("offline ingest must NOT fail on chtimes error; got %v", fr.Err)
	}
	if !fr.Queued {
		t.Error("expected Queued=true (the durability boundary must still complete)")
	}
	if !fr.LocalVerify.Verified {
		t.Error("expected local copy to verify")
	}

	// The mtime is set on the agent's temp copy before it is renamed into
	// place (the rename keeps it), so that is the destination logged.
	wantDest := localTempName(filepath.Join(dir, "local", "IMG_0001.jpg"))
	warn := findCHtimesWarn(t, logBuf, wantDest)
	if warn == nil {
		t.Fatalf("expected slog.Warn for local destination %q; got log: %s", wantDest, logBuf.String())
	}
	if warn["level"] != "WARN" {
		t.Errorf("warn level = %v, want WARN", warn["level"])
	}
	if warn["source"] != filepath.Join(cardRoot, "IMG_0001.jpg") {
		t.Errorf("warn source = %v", warn["source"])
	}
	errMsg, ok := warn["err"].(string)
	if !ok || !strings.Contains(errMsg, chtimesErr.Error()) {
		t.Errorf("warn err = %v, want something containing %q", warn["err"], chtimesErr)
	}
}

type timeoutCheckClient struct {
	fakeClient
}

func (timeoutCheckClient) CheckContent(ctx context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
	if fastHash == "" && fullHash == "" {
		// Run-level reachability probe succeeds so per-file check is reached
		return branchdam.ContentCheckResult{Found: false}, nil
	}
	<-ctx.Done()
	return branchdam.ContentCheckResult{}, ctx.Err()
}

type duplicateCheckClient struct {
	fakeClient
	nodeUUID       string
	filePath       string
	lifecycleState string
}

func (d duplicateCheckClient) CheckContent(ctx context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
	if fullHash == "" {
		return branchdam.ContentCheckResult{Found: true}, nil
	}
	state := d.lifecycleState
	if state == "" {
		state = "ACTIVE"
	}
	return branchdam.ContentCheckResult{
		Found:          true,
		NodeUUID:       d.nodeUUID,
		FilePath:       d.filePath,
		LifecycleState: state,
	}, nil
}

func TestIngestFileOfflineDedupTimeout(t *testing.T) {
	t.Run("per-file dedup pre-flight times out and falls open to normal offline ingest", func(t *testing.T) {
		buf := captureSlog(t)
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "IMG_TIMEOUT.jpg"), []byte("offline-timeout-content"), 0o644); err != nil {
			t.Fatal(err)
		}

		store := openStoreT(t)
		client := &timeoutCheckClient{}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newOfflineTestEngine(t, client, archiveRoot, localRoot, "/storage/staging/agent-1", store)
		// Set PreflightTimeoutSecs = 1 to test the per-request HTTP deadline
		e.Ingest.PreflightTimeoutSecs = 1

		start := time.Now()
		res, err := e.IngestCardOffline(context.Background(), cardRoot)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("IngestCardOffline: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped on timeout, got Skipped=true")
		}
		if !fr.Queued {
			t.Errorf("expected file to be Queued=true after fail-open offline write")
		}
		if !fr.LocalVerify.Verified {
			t.Errorf("expected local copy to verify")
		}
		if _, err := os.Stat(fr.LocalPath); err != nil {
			t.Errorf("local copy missing: %v", err)
		}
		if elapsed > 10*time.Second {
			t.Errorf("offline ingest took too long (%v), timeout should have bounded it", elapsed)
		}

		// Verify that the per-file timeout warning was logged
		logStr := buf.String()
		if !strings.Contains(logStr, "content check pre-flight failed (fail-open)") {
			t.Errorf("expected per-file timeout warning in logs, got:\n%s", logStr)
		}
	})

	t.Run("duplicate skips local write and queues nothing", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "IMG_DUP.jpg"), []byte("offline-duplicate-content"), 0o644); err != nil {
			t.Fatal(err)
		}

		store := openStoreT(t)
		client := &duplicateCheckClient{
			nodeUUID: "0190f1a2-offline-dup-uuid",
			filePath: "/storage/archive/2026/IMG_DUP.jpg",
		}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newOfflineTestEngine(t, client, archiveRoot, localRoot, "/storage/staging/agent-1", store)

		res, err := e.IngestCardOffline(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCardOffline: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if !fr.Skipped {
			t.Errorf("expected Skipped=true for duplicate, got false")
		}
		if fr.ExistingNodeUUID != "0190f1a2-offline-dup-uuid" {
			t.Errorf("ExistingNodeUUID = %q, want 0190f1a2-offline-dup-uuid", fr.ExistingNodeUUID)
		}
		wantReason := "duplicate: already in library as node 0190f1a2-offline-dup-uuid at /storage/archive/2026/IMG_DUP.jpg"
		if fr.SkipReason != wantReason {
			t.Errorf("SkipReason = %q, want %q", fr.SkipReason, wantReason)
		}
		if fr.Queued {
			t.Errorf("expected Queued=false for duplicate")
		}
		// Ensure local file was never written
		if _, err := os.Stat(fr.LocalPath); !os.IsNotExist(err) {
			t.Errorf("local file should not exist, err=%v", err)
		}
	})

	t.Run("duplicate with non-live lifecycle state proceeds with local write", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "IMG_ARCHIVED.jpg"), []byte("offline-archived-content"), 0o644); err != nil {
			t.Fatal(err)
		}

		store := openStoreT(t)
		client := &duplicateCheckClient{
			nodeUUID:       "0190f1a2-offline-archived-uuid",
			filePath:       "/storage/archive/2026/IMG_ARCHIVED.jpg",
			lifecycleState: "ARCHIVED",
		}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newOfflineTestEngine(t, client, archiveRoot, localRoot, "/storage/staging/agent-1", store)

		res, err := e.IngestCardOffline(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCardOffline: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		fr := res.Files[0]
		if fr.Skipped {
			t.Errorf("expected non-live (ARCHIVED) duplicate to NOT be skipped")
		}
		if !fr.Queued {
			t.Errorf("expected file to be Queued=true")
		}
	})

	t.Run("HTTPError per-file error does not latch dedupUnavailable", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "IMG_1.jpg"), []byte("file1"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "IMG_2.jpg"), []byte("file2"), 0o644); err != nil {
			t.Fatal(err)
		}

		store := openStoreT(t)
		callCount := 0
		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
				if fastHash == "" && fullHash == "" {
					// Probe succeeds
					return branchdam.ContentCheckResult{}, nil
				}
				callCount++
				// Return 500 HTTPError (server reachable, per-request error)
				return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: http.StatusInternalServerError, Body: "internal error"}
			},
		}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newOfflineTestEngine(t, client, archiveRoot, localRoot, "/storage/staging/agent-1", store)

		res, err := e.IngestCardOffline(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCardOffline: %v", err)
		}
		if len(res.Files) != 2 {
			t.Fatalf("got %d files, want 2", len(res.Files))
		}
		// Since HTTPError (500) is per-request and does not latch, both files should attempt preflight check
		if callCount != 2 {
			t.Errorf("expected 2 preflight attempts (not latched on 500), got %d", callCount)
		}
	})

	t.Run("AlreadyIngested destination match skips pre-flight check", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		dir1 := filepath.Join(cardRoot, "DCIM", "100MSDCF")
		dir2 := filepath.Join(cardRoot, "DCIM", "101MSDCF")
		if err := os.MkdirAll(dir1, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir2, 0o755); err != nil {
			t.Fatal(err)
		}
		content := []byte("already-existing-content")
		if err := os.WriteFile(filepath.Join(dir1, "IMG_DUP.jpg"), content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir2, "IMG_DUP.jpg"), content, 0o644); err != nil {
			t.Fatal(err)
		}

		store := openStoreT(t)
		callCount := 0
		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
				if fastHash == "" && fullHash == "" {
					// Probe succeeds
					return branchdam.ContentCheckResult{}, nil
				}
				callCount++
				return branchdam.ContentCheckResult{Found: false}, nil
			},
		}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newOfflineTestEngine(t, client, archiveRoot, localRoot, "/storage/staging/agent-1", store)
		e.Ingest.PathTemplate = "{original_name}"

		res, err := e.IngestCardOffline(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCardOffline: %v", err)
		}
		if len(res.Files) != 2 {
			t.Fatalf("got %d files, want 2", len(res.Files))
		}

		var queued, skipped int
		for _, f := range res.Files {
			if f.Err != nil {
				t.Fatalf("unexpected error: %v", f.Err)
			}
			if f.Skipped {
				skipped++
				if f.SkipReason != "already ingested (identical file exists at destination)" {
					t.Errorf("SkipReason = %q, want 'already ingested (identical file exists at destination)'", f.SkipReason)
				}
			} else if f.Queued {
				queued++
			}
		}
		if queued != 1 || skipped != 1 {
			t.Fatalf("got queued=%d skipped=%d, want 1 and 1", queued, skipped)
		}
		// First file called pre-flight (callCount=1); second file was AlreadyIngested so it skipped pre-flight check (callCount stays 1)
		if callCount != 1 {
			t.Errorf("expected exactly 1 CheckContent call (second file skipped pre-flight via AlreadyIngested), got %d", callCount)
		}
	})
}

// TestIngestCardOfflineReformattedCardIsNotAResume is the regression test
// for the "reformatted card reads as already queued" data-loss bug: a
// camera restarts its file counter after a format, so a second card
// mounted at the same path yields the same source paths with different
// content. queue.db rows are never deleted, so a source-path-only match
// used to report every such file as AlreadyQueued without copying it --
// the summary read as success, auto-eject fired, and the photos were only
// ever on the card. A path match must count as a resume only when the
// stored size and mtime also match.
func TestIngestCardOfflineReformattedCardIsNotAResume(t *testing.T) {
	cases := []struct {
		name          string
		first, second string
		secondMtime   time.Time
	}{
		{
			name:        "same size, different mtime",
			first:       "card-one-bytes",
			second:      "card-two-bytes",
			secondMtime: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		},
		{
			name:        "different size, same mtime",
			first:       "card-one-bytes",
			second:      "card-two-has-more-bytes",
			secondMtime: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		},
	}
	firstMtime := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Both "cards" mount at the same path, as two SD cards from the
			// same camera do (/Volumes/EOS_DIGITAL).
			cardRoot := filepath.Join(dir, "EOS_DIGITAL")
			srcRel := filepath.Join("DCIM", "100CANON", "IMG_0001.jpg")
			srcPath := filepath.Join(cardRoot, srcRel)
			writeCardFile := func(content string, mtime time.Time) {
				t.Helper()
				if err := os.RemoveAll(cardRoot); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(srcPath, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(srcPath, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}

			localRoot := filepath.Join(dir, "local")
			store := openStoreT(t)
			e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), localRoot, "/storage/staging/agent-1", store)
			e.Ingest.PathTemplate = "{original_name}"
			var minted int
			e.NewNodeUUID = func() (string, error) {
				minted++
				return fmt.Sprintf("node-%d", minted), nil
			}

			writeCardFile(tc.first, firstMtime)
			res1, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil {
				t.Fatal(err)
			}
			if len(res1.Files) != 1 || res1.Files[0].Outcome() != OfflineOutcomeQueued {
				t.Fatalf("first card: got %+v, want one freshly queued file", res1.Files)
			}

			// Second card, same path, different file.
			writeCardFile(tc.second, tc.secondMtime)
			res2, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil {
				t.Fatal(err)
			}
			if len(res2.Files) != 1 {
				t.Fatalf("second card: got %d files, want 1", len(res2.Files))
			}
			fr2 := res2.Files[0]
			if fr2.Err != nil {
				t.Fatalf("second card: unexpected error: %v", fr2.Err)
			}
			if fr2.AlreadyQueued {
				t.Fatal("second card's file was reported AlreadyQueued -- a different file at the same source path is not a resume")
			}
			if got := fr2.Outcome(); got != OfflineOutcomeQueued {
				t.Fatalf("second card: Outcome() = %v, want OfflineOutcomeQueued", got)
			}
			if fr2.NodeUUID == res1.Files[0].NodeUUID {
				t.Error("second card's file reused the first card's NodeUUID")
			}
			if fr2.LocalPath == res1.Files[0].LocalPath {
				t.Errorf("second card's file landed on the first card's local path %s", fr2.LocalPath)
			}

			// Both files exist locally with their own content.
			for path, want := range map[string]string{res1.Files[0].LocalPath: tc.first, fr2.LocalPath: tc.second} {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("local copy %s: %v", path, err)
				}
				if string(got) != want {
					t.Errorf("local copy %s = %q, want %q", path, got, want)
				}
			}

			all, err := store.All(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 2 {
				t.Fatalf("got %d queue rows, want 2 (one per card)", len(all))
			}

			// A genuine resume of the second card (same file, e.g. a re-run
			// after a crash) still resumes against the newest row: no new
			// copy, no new row, no new NodeUUID.
			e.NewNodeUUID = func() (string, error) {
				t.Fatal("NewNodeUUID must not be called when resuming an already-queued file")
				return "", nil
			}
			res3, err := e.IngestCardOffline(context.Background(), cardRoot)
			if err != nil {
				t.Fatal(err)
			}
			fr3 := res3.Files[0]
			if got := fr3.Outcome(); got != OfflineOutcomeResumed {
				t.Fatalf("re-run: Outcome() = %v, want OfflineOutcomeResumed (err=%v)", got, fr3.Err)
			}
			if fr3.NodeUUID != fr2.NodeUUID {
				t.Errorf("re-run resumed NodeUUID %q, want the second card's %q", fr3.NodeUUID, fr2.NodeUUID)
			}
			all, err = store.All(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 2 {
				t.Fatalf("got %d queue rows after resume, want still 2", len(all))
			}
		})
	}
}

func TestOfflineFileResultOutcome(t *testing.T) {
	cases := []struct {
		name string
		fr   OfflineFileResult
		want OfflineOutcome
	}{
		{"error wins", OfflineFileResult{Err: errors.New("boom"), Queued: true, AlreadyQueued: true}, OfflineOutcomeFailed},
		{"fresh queue", OfflineFileResult{Queued: true}, OfflineOutcomeQueued},
		{"resume", OfflineFileResult{Queued: true, AlreadyQueued: true}, OfflineOutcomeResumed},
		{"resumed sidecar", OfflineFileResult{Queued: true, AlreadyQueued: true, Skipped: true, IsSidecar: true}, OfflineOutcomeResumed},
		{"fresh sidecar", OfflineFileResult{Queued: true, Skipped: true, IsSidecar: true}, OfflineOutcomeSkipped},
		{"deliberate skip", OfflineFileResult{Skipped: true, SkipReason: "OS metadata"}, OfflineOutcomeSkipped},
		{"neither written nor resumed", OfflineFileResult{SourcePath: "a.jpg"}, OfflineOutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fr.Outcome(); got != tc.want {
				t.Errorf("Outcome() = %v, want %v", got, tc.want)
			}
		})
	}
}
