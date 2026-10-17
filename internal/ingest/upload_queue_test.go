package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// drainUploadClient is a drainClient that can also upload, scriptable per
// call: the fake /upload endpoint of Drain's upload phase.
type drainUploadClient struct {
	fakeDrainClient
	uploads    []branchdam.UploadOptions
	bodies     [][]byte
	relPath    func(opts branchdam.UploadOptions) string
	uploadErr  func(call int, opts branchdam.UploadOptions) error
	beforeRead func(opts branchdam.UploadOptions)
	// slow, when set, makes Upload wait that long before answering and fail
	// with the context's error if the context ended meanwhile.
	slow time.Duration
}

func (u *drainUploadClient) Upload(ctx context.Context, r io.Reader, opts branchdam.UploadOptions) (*branchdam.UploadResponse, error) {
	if u.beforeRead != nil {
		u.beforeRead(opts)
	}
	u.uploads = append(u.uploads, opts)
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	u.bodies = append(u.bodies, body)
	if u.slow > 0 {
		select {
		case <-time.After(u.slow):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if u.uploadErr != nil {
		if err := u.uploadErr(len(u.uploads), opts); err != nil {
			return nil, err
		}
	}
	rel := "2026/" + opts.Filename
	if u.relPath != nil {
		rel = u.relPath(opts)
	}
	return &branchdam.UploadResponse{
		NodeUUID:     fmt.Sprintf("srv-node-%d", len(u.uploads)),
		Status:       "UPLOADED",
		BytesWritten: int64(len(body)),
		Blake3Hash:   blake3Bytes(body),
		RelativePath: rel,
	}, nil
}

func openUploadQueue(t *testing.T) (*queue.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := queue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// meteredQueueEngine is an upload-mode engine held back by a metered link,
// with a queue attached.
func meteredQueueEngine(t *testing.T, localRoot string, store *queue.Store) *Engine {
	t.Helper()
	e := newUploadEngine(&recordingUploader{}, localRoot)
	e.Ingest.PauseUploadOnMetered = true
	e.IsMetered = func() (bool, error) { return true, nil }
	e.Queue = store
	return e
}

func ingestOne(t *testing.T, e *Engine, card string) []FileResult {
	t.Helper()
	res, err := e.IngestCard(context.Background(), card)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	return res.Files
}

func allRows(t *testing.T, store *queue.Store) []queue.Record {
	t.Helper()
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func rowByNode(t *testing.T, store *queue.Store, node string) queue.Record {
	t.Helper()
	r, ok, err := store.ByNodeUUID(context.Background(), node)
	if err != nil || !ok {
		t.Fatalf("ByNodeUUID(%s): ok=%v err=%v", node, ok, err)
	}
	return r
}

// TestMeteredDeferralQueuesAnUploadRow: with a queue attached, a metered
// deferral is recorded in queue.db before anything else happens, and the
// file reports Queued (not Deferred): the card is no longer needed.
func TestMeteredDeferralQueuesAnUploadRow(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	src := writeUploadCardFile(t, card, "IMG_0001.JPG", "metered bytes", time.Time{})
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)

	files := ingestOne(t, e, card)
	fr := files[0]
	if fr.Err != nil || !fr.Queued || fr.Deferred || fr.Skipped || fr.UploadQueueID == "" {
		t.Fatalf("result = %+v, want Queued (not Deferred/Skipped) with an UploadQueueID", fr)
	}

	rows := allRows(t, store)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	local := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	if r.NodeUUID != fr.UploadQueueID || r.Origin != queue.OriginUpload || r.Kind != queue.KindMedia {
		t.Errorf("row = %+v", r)
	}
	if r.UploadStatus != queue.StatusPending || r.NodeCreatedStatus != queue.StatusSkipped ||
		r.ArchiveCopyStatus != queue.StatusSkipped || r.RebaseStatus != queue.StatusSkipped {
		t.Errorf("statuses: upload=%s node_created=%s archive=%s rebase=%s", r.UploadStatus, r.NodeCreatedStatus, r.ArchiveCopyStatus, r.RebaseStatus)
	}
	if r.LocalPath != local || r.LocalEditRoot != localRoot || r.SourcePath != src {
		t.Errorf("paths: local=%q root=%q source=%q", r.LocalPath, r.LocalEditRoot, r.SourcePath)
	}
	if r.FullHash != blake3Bytes([]byte("metered bytes")) || r.SizeBytes != int64(len("metered bytes")) || r.FastHash == "" {
		t.Errorf("hashes/size: %+v", r)
	}
	if r.UploadName != "IMG_0001.JPG" || r.SourcePathHash != sourcePathHash(src) || r.CaptureTimestamp == 0 {
		t.Errorf("upload inputs: name=%q sph=%q capture=%d", r.UploadName, r.SourcePathHash, r.CaptureTimestamp)
	}
	if r.Done() {
		t.Error("a pending upload row must not be Done")
	}
}

// TestMeteredDeferralStaysDeferredWhenTheQueueInsertFails: no row means the
// card is still the only other copy, so the file keeps today's Deferred
// semantics.
func TestMeteredDeferralStaysDeferredWhenTheQueueInsertFails(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "metered bytes", time.Time{})
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	fr := ingestOne(t, e, card)[0]
	if fr.Err != nil || !fr.Deferred || !fr.Skipped || fr.Queued || fr.UploadQueueID != "" {
		t.Fatalf("result = %+v, want plain Deferred", fr)
	}
}

// TestMeteredDeferralIsIdempotent: re-inserting the card while still
// metered does not queue the file twice.
func TestMeteredDeferralIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "metered bytes", time.Time{})
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)

	first := ingestOne(t, e, card)[0]
	second := ingestOne(t, e, card)[0]
	if !first.Queued || !second.Queued || first.UploadQueueID != second.UploadQueueID {
		t.Fatalf("first=%+v second=%+v, want both Queued against the same row", first, second)
	}
	if n := len(allRows(t, store)); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestPreexistingDeferredCopyGetsARow: a copy a pre-queue deferral left in
// place (no row) is hashed from the local edit copy and queued.
func TestPreexistingDeferredCopyGetsARow(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "metered bytes", time.Time{})
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)
	e.Queue = nil
	if fr := ingestOne(t, e, card)[0]; !fr.Deferred {
		t.Fatalf("setup: %+v, want Deferred", fr)
	}

	e.Queue = store
	fr := ingestOne(t, e, card)[0]
	if !fr.Queued || fr.UploadQueueID == "" {
		t.Fatalf("result = %+v, want Queued", fr)
	}
	rows := allRows(t, store)
	if len(rows) != 1 || rows[0].FullHash != blake3Bytes([]byte("metered bytes")) {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestDrainUploadsAQueuedDeferralWithoutTheCard is the acceptance test: the
// deferral survives a restart (the store is reopened), the card is gone, and
// Drain uploads the verified local copy and moves it to the server's path.
func TestDrainUploadsAQueuedDeferralWithoutTheCard(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	src := writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, dbPath := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)
	node := ingestOne(t, e, card)[0].UploadQueueID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(card); err != nil {
		t.Fatal(err)
	}

	store2, err := queue.Open(dbPath) // "restart"
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	client := &drainUploadClient{relPath: func(o branchdam.UploadOptions) string { return "2026/2026-08-29/" + o.Filename }}
	stats, err := Drain(context.Background(), client, store2, "agent-1", nil)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if stats.UploadsDone != 1 || stats.UploadsFailed != 0 || stats.Remaining != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(client.uploads) != 1 {
		t.Fatalf("uploads = %d, want 1", len(client.uploads))
	}
	o := client.uploads[0]
	if o.Filename != "IMG_0001.JPG" || o.Blake3Hash != blake3Bytes([]byte("queued bytes")) ||
		o.SourcePathHash != sourcePathHash(src) || o.SizeBytes != int64(len("queued bytes")) || o.CaptureTimestamp == 0 {
		t.Errorf("upload opts = %+v", o)
	}
	if string(client.bodies[0]) != "queued bytes" {
		t.Errorf("body = %q", client.bodies[0])
	}

	moved := filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0001.JPG")
	if b, err := os.ReadFile(moved); err != nil || string(b) != "queued bytes" {
		t.Errorf("moved copy: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "2026", "IMG_0001.JPG")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy is still at its deferred name: %v", err)
	}
	r := rowByNode(t, store2, node)
	if r.UploadStatus != queue.StatusDone || r.ServerNodeUUID != "srv-node-1" || r.ServerRelativePath != "2026/2026-08-29/IMG_0001.JPG" || r.LocalPath != moved {
		t.Errorf("row = %+v", r)
	}
	if !r.Done() || r.Failed() {
		t.Errorf("Done=%v Failed=%v, want done and not failed", r.Done(), r.Failed())
	}

	// A later pass has nothing left to upload.
	if _, err := Drain(context.Background(), client, store2, "agent-1", nil); err != nil {
		t.Fatal(err)
	}
	if len(client.uploads) != 1 {
		t.Errorf("uploads after second pass = %d, want still 1", len(client.uploads))
	}
}

// TestDrainUploadKeepsACopyWhoseServerPathIsTaken: nothing is ever
// overwritten -- the upload still counts, the file stays, and the row records
// where it actually is.
func TestDrainUploadKeepsACopyWhoseServerPathIsTaken(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID

	taken := filepath.Join(localRoot, "elsewhere", "IMG_0001.JPG")
	if err := os.MkdirAll(filepath.Dir(taken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taken, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &drainUploadClient{relPath: func(o branchdam.UploadOptions) string { return "elsewhere/" + o.Filename }}
	stats, err := Drain(context.Background(), client, store, "agent-1", nil)
	if err != nil || stats.UploadsDone != 1 {
		t.Fatalf("stats = %+v err = %v", stats, err)
	}
	deferred := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	if b, _ := os.ReadFile(deferred); string(b) != "queued bytes" {
		t.Errorf("the local copy moved or changed: %q", b)
	}
	if b, _ := os.ReadFile(taken); string(b) != "someone else's file" {
		t.Errorf("the file at the server's path was overwritten: %q", b)
	}
	if r := rowByNode(t, store, node); r.UploadStatus != queue.StatusDone || r.LocalPath != deferred {
		t.Errorf("row = %+v, want DONE with the unchanged local path", r)
	}
}

func TestDrainUploadFailsWhenTheLocalCopyChanged(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID

	local := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	if err := os.WriteFile(local, []byte("edited bytes"), 0o644); err != nil { // same size
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(local, later, later); err != nil {
		t.Fatal(err)
	}
	client := &drainUploadClient{}
	stats, err := Drain(context.Background(), client, store, "agent-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UploadsFailed != 1 || stats.UploadsDone != 0 || len(client.uploads) != 0 {
		t.Fatalf("stats = %+v uploads = %d, want one failed row and no upload", stats, len(client.uploads))
	}
	r := rowByNode(t, store, node)
	if r.UploadStatus != queue.StatusFailed || r.UploadLastError == "" || !r.Failed() {
		t.Errorf("row = %+v", r)
	}

	// Gone entirely: fails too (after a reset), never uploads.
	if _, err := store.ResetFailed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if _, err := Drain(context.Background(), client, store, "agent-1", nil); err != nil {
		t.Fatal(err)
	}
	if r := rowByNode(t, store, node); r.UploadStatus != queue.StatusFailed || len(client.uploads) != 0 {
		t.Errorf("row = %+v uploads = %d", r, len(client.uploads))
	}
}

func TestDrainUploadErrorClassification(t *testing.T) {
	// A Huma problem document on a request-shaped status: the same fixture
	// the rebase classification test uses.
	fatal := &branchdam.HTTPError{StatusCode: 409, Body: `{"title":"Conflict","status":409,"detail":"full hash differs"}`}
	cases := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{"transient network error", errors.New("connection reset"), queue.StatusPending},
		{"server 503", &branchdam.HTTPError{StatusCode: 503}, queue.StatusPending},
		{"stalled upload", branchdam.ErrUploadStalled, queue.StatusPending},
		{"hash mismatch", fmt.Errorf("x: %w", branchdam.ErrUploadHashMismatch), queue.StatusFailed},
		{"fatal http status", fatal, queue.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
			writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
			store, _ := openUploadQueue(t)
			node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID

			client := &drainUploadClient{uploadErr: func(int, branchdam.UploadOptions) error { return tc.err }}
			now := time.Now()
			if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(now)); err != nil {
				t.Fatal(err)
			}
			r := rowByNode(t, store, node)
			if r.UploadStatus != tc.wantStatus {
				t.Fatalf("status = %s, want %s (row %+v)", r.UploadStatus, tc.wantStatus, r)
			}
			if tc.wantStatus == queue.StatusPending {
				if r.UploadAttempts != 1 || r.UploadLastError == "" || r.UploadNextAttemptUnix <= now.Unix() {
					t.Errorf("attempt not recorded with backoff: %+v", r)
				}
				// Within the backoff window the next pass leaves it alone.
				if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(now)); err != nil {
					t.Fatal(err)
				}
				if len(client.uploads) != 1 {
					t.Errorf("uploads = %d, want the backoff to hold the retry", len(client.uploads))
				}
			}
			if _, err := os.Stat(filepath.Join(localRoot, "2026", "IMG_0001.JPG")); err != nil {
				t.Errorf("the local copy must survive a failed upload: %v", err)
			}
		})
	}
}

// TestDrainUploadGateHoldsUploadsOnAMeteredLink: the gate fails closed and
// holds only the upload phase.
func TestDrainUploadGateHoldsUploadsOnAMeteredLink(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID
	client := &drainUploadClient{}

	for name, gate := range map[string]func() (bool, error){
		"metered":     func() (bool, error) { return true, nil },
		"probe error": func() (bool, error) { return false, errors.New("no network manager") },
	} {
		if _, err := Drain(context.Background(), client, store, "agent-1", nil, WithUploadGate(gate)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(client.uploads) != 0 || rowByNode(t, store, node).UploadStatus != queue.StatusPending {
			t.Fatalf("%s: uploads = %d, want the row held", name, len(client.uploads))
		}
	}
	if _, err := Drain(context.Background(), client, store, "agent-1", nil,
		WithUploadGate(func() (bool, error) { return false, nil })); err != nil {
		t.Fatal(err)
	}
	if len(client.uploads) != 1 {
		t.Errorf("uploads = %d, want 1 once unmetered", len(client.uploads))
	}
}

// TestDrainUploadIsNotCutOffByTheShortPassDeadline: the pass context's
// deadline (the tray's is two minutes) must not cancel a long upload when an
// upload context is supplied -- and does without one.
func TestDrainUploadIsNotCutOffByTheShortPassDeadline(t *testing.T) {
	setup := func(t *testing.T) (*queue.Store, string) {
		dir := t.TempDir()
		card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
		writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
		store, _ := openUploadQueue(t)
		return store, ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID
	}

	t.Run("with an upload context", func(t *testing.T) {
		store, node := setup(t)
		pass, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		client := &drainUploadClient{slow: 200 * time.Millisecond}
		stats, err := Drain(pass, client, store, "agent-1", nil, WithUploadContext(context.Background()))
		if err != nil {
			t.Fatalf("Drain: %v (the closing bookkeeping must not use the spent pass context)", err)
		}
		if stats.UploadsDone != 1 || rowByNode(t, store, node).UploadStatus != queue.StatusDone {
			t.Fatalf("stats = %+v", stats)
		}
	})
	t.Run("without one the pass deadline applies", func(t *testing.T) {
		store, node := setup(t)
		pass, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		client := &drainUploadClient{slow: 200 * time.Millisecond}
		_, _ = Drain(pass, client, store, "agent-1", nil)
		r := rowByNode(t, store, node)
		if r.UploadStatus != queue.StatusPending || r.UploadAttempts != 0 {
			t.Errorf("row = %+v, want an untouched PENDING row (a cancelled pass is not a failed attempt)", r)
		}
	})
}

// TestDrainSkipsAnUploadAReinsertAlreadyFinished: Drain holds no ingest gate,
// so a card re-insert can finish the same row while Drain streams. Whoever
// finishes second must leave the local copy alone.
func TestDrainSkipsAnUploadAReinsertAlreadyFinished(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID
	deferred := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	reinsertPath := filepath.Join(localRoot, "2026", "from-reinsert", "IMG_0001.JPG")

	client := &drainUploadClient{relPath: func(o branchdam.UploadOptions) string { return "2026/drain/" + o.Filename }}
	client.beforeRead = func(branchdam.UploadOptions) {
		// The re-insert's reconcile lands while Drain's stream is in flight.
		ok, err := store.MarkUploadDone(context.Background(), node, "srv-from-reinsert", "2026/from-reinsert/IMG_0001.JPG", reinsertPath)
		if err != nil || !ok {
			t.Errorf("simulated reconcile: ok=%v err=%v", ok, err)
		}
	}
	stats, err := Drain(context.Background(), client, store, "agent-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UploadsDone != 0 {
		t.Errorf("UploadsDone = %d, want the finished row left alone", stats.UploadsDone)
	}
	if _, err := os.Stat(deferred); err != nil {
		t.Errorf("Drain moved a copy the re-insert owns: %v", err)
	}
	if r := rowByNode(t, store, node); r.ServerNodeUUID != "srv-from-reinsert" || r.LocalPath != reinsertPath {
		t.Errorf("row = %+v, want the re-insert's record untouched", r)
	}
}

// TestReinsertUploadClosesTheQueuedRow: an unmetered re-insert uploads the
// file from the card itself and closes the row, so Drain never uploads it a
// second time.
func TestReinsertUploadClosesTheQueuedRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relPath   string
		wantLocal string
	}{
		{"server path equals the deferred name", "2026/IMG_0001.JPG", "2026/IMG_0001.JPG"},
		{"server path differs: the deferred copy moves", "2026/2026-08-29/IMG_0001.JPG", "2026/2026-08-29/IMG_0001.JPG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
			writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
			store, _ := openUploadQueue(t)
			e := meteredQueueEngine(t, localRoot, store)
			node := ingestOne(t, e, card)[0].UploadQueueID

			u := &recordingUploader{relPath: func(branchdam.UploadOptions) string { return tc.relPath }}
			e.Uploader = u
			e.IsMetered = func() (bool, error) { return false, nil }
			fr := ingestOne(t, e, card)[0]
			if fr.Err != nil || fr.Deferred || fr.Queued {
				t.Fatalf("re-insert result = %+v, want a plain upload", fr)
			}
			r := rowByNode(t, store, node)
			if r.UploadStatus != queue.StatusDone || r.ServerNodeUUID != "node-1" || r.ServerRelativePath != tc.relPath ||
				r.LocalPath != filepath.Join(localRoot, tc.wantLocal) {
				t.Fatalf("row = %+v", r)
			}

			client := &drainUploadClient{}
			if _, err := Drain(context.Background(), client, store, "agent-1", nil); err != nil {
				t.Fatal(err)
			}
			if len(client.uploads) != 0 {
				t.Errorf("Drain re-uploaded a file the re-insert already uploaded")
			}
		})
	}
}

// TestDeferredSidecarFollowsItsMasterThroughTheDrain: the sidecar's row links
// to the master's; Drain waits for the master and names the sidecar after the
// master's server-assigned name (a collision suffix included), and a failed
// master fails it too.
func TestDeferredSidecarFollowsItsMasterThroughTheDrain(t *testing.T) {
	setup := func(t *testing.T) (*queue.Store, []queue.Record, string) {
		dir := t.TempDir()
		card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
		writeUploadCardFile(t, card, "IMG_0001.JPG", "master bytes", time.Time{})
		writeUploadCardFile(t, card, "IMG_0001.xmp", "sidecar bytes", time.Time{})
		store, _ := openUploadQueue(t)
		files := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)
		for _, f := range files {
			if !f.Queued {
				t.Fatalf("file %s = %+v, want Queued", f.SourcePath, f)
			}
		}
		rows := allRows(t, store)
		if len(rows) != 2 || rows[0].Kind != queue.KindMedia || rows[1].Kind != queue.KindSidecar {
			t.Fatalf("rows = %+v, want master then sidecar", rows)
		}
		if rows[1].MasterNodeUUID != rows[0].NodeUUID || rows[1].SidecarTail != ".xmp" || rows[1].SidecarByName {
			t.Fatalf("sidecar row link = %+v", rows[1])
		}
		return store, rows, localRoot
	}

	t.Run("named after the master's server name", func(t *testing.T) {
		store, rows, localRoot := setup(t)
		client := &drainUploadClient{relPath: func(o branchdam.UploadOptions) string {
			if o.Filename == "IMG_0001.JPG" {
				return "2026/2026-08-29/IMG_0001_2.JPG" // the server suffixed the master
			}
			return "2026/2026-08-29/" + o.Filename
		}}
		stats, err := Drain(context.Background(), client, store, "agent-1", nil)
		if err != nil || stats.UploadsDone != 2 {
			t.Fatalf("stats = %+v err = %v", stats, err)
		}
		if len(client.uploads) != 2 || client.uploads[0].Filename != "IMG_0001.JPG" || client.uploads[1].Filename != "IMG_0001_2.xmp" {
			t.Fatalf("uploads = %+v, want the master, then the sidecar under the master's server stem", client.uploads)
		}
		if client.uploads[1].CaptureTimestamp != client.uploads[0].CaptureTimestamp || client.uploads[1].CameraModel != client.uploads[0].CameraModel {
			t.Errorf("sidecar did not inherit the master's capture inputs: %+v vs %+v", client.uploads[1], client.uploads[0])
		}
		if r := rowByNode(t, store, rows[1].NodeUUID); r.UploadStatus != queue.StatusDone ||
			r.LocalPath != filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0001_2.xmp") {
			t.Errorf("sidecar row = %+v", r)
		}
	})

	t.Run("waits while the master backs off", func(t *testing.T) {
		store, rows, _ := setup(t)
		client := &drainUploadClient{uploadErr: func(int, branchdam.UploadOptions) error { return errors.New("connection reset") }}
		if _, err := Drain(context.Background(), client, store, "agent-1", nil); err != nil {
			t.Fatal(err)
		}
		if len(client.uploads) != 1 {
			t.Fatalf("uploads = %d, want only the master attempted", len(client.uploads))
		}
		if r := rowByNode(t, store, rows[1].NodeUUID); r.UploadStatus != queue.StatusPending || r.UploadAttempts != 0 {
			t.Errorf("sidecar row = %+v, want untouched while its master is pending", r)
		}
	})

	t.Run("fails when the master failed", func(t *testing.T) {
		store, rows, _ := setup(t)
		if err := store.MarkUploadFailed(context.Background(), rows[0].NodeUUID, "boom"); err != nil {
			t.Fatal(err)
		}
		client := &drainUploadClient{}
		stats, err := Drain(context.Background(), client, store, "agent-1", nil)
		if err != nil || stats.UploadsFailed != 1 || len(client.uploads) != 0 {
			t.Fatalf("stats = %+v uploads = %d err = %v", stats, len(client.uploads), err)
		}
		if r := rowByNode(t, store, rows[1].NodeUUID); r.UploadStatus != queue.StatusFailed {
			t.Errorf("sidecar row = %+v", r)
		}
		// Resetting re-queues both, so the master goes first again.
		counts, err := store.ResetFailed(context.Background())
		if err != nil || counts.Uploads != 2 {
			t.Fatalf("ResetFailed = %+v err = %v, want both uploads reset", counts, err)
		}
	})
}

// TestSidecarOfAMasterDeferredWithoutARowIsNotQueued: a sidecar row would
// upload alone, detached from its master.
func TestSidecarOfAMasterDeferredWithoutARowIsNotQueued(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "master bytes", time.Time{})
	writeUploadCardFile(t, card, "IMG_0001.xmp", "sidecar bytes", time.Time{})
	e := meteredQueueEngine(t, localRoot, nil) // no queue: the master stays Deferred
	for _, f := range ingestOne(t, e, card) {
		if !f.Deferred || f.Queued {
			t.Errorf("file %s = %+v, want plain Deferred", f.SourcePath, f)
		}
	}
}

// TestDrainWithAClientThatCannotUploadHasNoUploadPhase: the existing Drain
// call sites (and their fakes) keep working.
func TestDrainWithAClientThatCannotUploadHasNoUploadPhase(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	node := ingestOne(t, meteredQueueEngine(t, localRoot, store), card)[0].UploadQueueID

	stats, err := Drain(context.Background(), &fakeDrainClient{}, store, "agent-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.UploadsDone != 0 || stats.Remaining != 1 || rowByNode(t, store, node).UploadStatus != queue.StatusPending {
		t.Errorf("stats = %+v", stats)
	}
}

// TestReinsertFromAnotherMountClosesTheMovedCopysRow: the card comes back
// under another path (or another card holds the same bytes), so the row's
// source path no longer matches. The re-insert still moves the deferred copy
// to the server's path -- the row that claimed the moved copy must be closed,
// or Drain would later fail it for a "missing" local copy.
func TestReinsertFromAnotherMountClosesTheMovedCopysRow(t *testing.T) {
	dir := t.TempDir()
	cardA, cardB, localRoot := filepath.Join(dir, "cardA"), filepath.Join(dir, "cardB"), filepath.Join(dir, "local")
	mtime := time.Now().Add(-time.Hour)
	writeUploadCardFile(t, cardA, "IMG_0001.JPG", "queued bytes", mtime)
	writeUploadCardFile(t, cardB, "IMG_0001.JPG", "queued bytes", mtime)
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)
	node := ingestOne(t, e, cardA)[0].UploadQueueID

	e.Uploader = &recordingUploader{relPath: func(branchdam.UploadOptions) string { return "2026/2026-08-29/IMG_0001.JPG" }}
	e.IsMetered = func() (bool, error) { return false, nil }
	if fr := ingestOne(t, e, cardB)[0]; fr.Err != nil || fr.Queued || fr.Deferred {
		t.Fatalf("re-insert result = %+v", fr)
	}
	moved := filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0001.JPG")
	r := rowByNode(t, store, node)
	if r.UploadStatus != queue.StatusDone || r.LocalPath != moved {
		t.Fatalf("row = %+v, want DONE at %s", r, moved)
	}
	client := &drainUploadClient{}
	stats, err := Drain(context.Background(), client, store, "agent-1", nil)
	if err != nil || stats.UploadsFailed != 0 || len(client.uploads) != 0 {
		t.Errorf("stats = %+v uploads = %d err = %v, want Drain to leave the closed row alone", stats, len(client.uploads), err)
	}
}

// TestMeteredDeferralOfACopyAnotherRowOwnsStaysDeferred: a drained upload
// would move a copy out from under an offline/online row that claims it.
func TestMeteredDeferralOfACopyAnotherRowOwnsStaysDeferred(t *testing.T) {
	dir := t.TempDir()
	card, localRoot := filepath.Join(dir, "card"), filepath.Join(dir, "local")
	writeUploadCardFile(t, card, "IMG_0001.JPG", "queued bytes", time.Time{})
	store, _ := openUploadQueue(t)
	e := meteredQueueEngine(t, localRoot, store)
	e.Queue = nil
	if fr := ingestOne(t, e, card)[0]; !fr.Deferred {
		t.Fatalf("setup: %+v", fr)
	}
	local := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID: "offline-owner", Kind: queue.KindMedia, SourcePath: "/other/IMG_0001.JPG", LocalPath: local,
		ArchivePath: "/archive/IMG_0001.JPG", FileName: "IMG_0001.JPG", FileExt: "jpg", SizeBytes: 12,
	}); err != nil {
		t.Fatal(err)
	}

	e.Queue = store
	fr := ingestOne(t, e, card)[0]
	if !fr.Deferred || fr.Queued || fr.UploadQueueID != "" {
		t.Fatalf("result = %+v, want plain Deferred", fr)
	}
	if n := len(allRows(t, store)); n != 1 {
		t.Errorf("rows = %d, want only the owner's", n)
	}
}
