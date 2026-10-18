package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// uploadQueueFixture queues one metered-deferred upload row whose local copy
// really exists, and returns the row's key, local path and content hash.
func uploadQueueFixture(t *testing.T, store *queue.Store, content string) (node, local, hash string) {
	t.Helper()
	return uploadQueueRow(t, store, content, "up-queued-1", "IMG_0001.JPG")
}

// uploadQueueRow queues one more origin = UPLOAD row with its own node and
// file name.
func uploadQueueRow(t *testing.T, store *queue.Store, content, node, name string) (_, local, hash string) {
	t.Helper()
	root := t.TempDir()
	local = filepath.Join(root, "2026", name)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	h := blake3.New()
	_, _ = h.Write([]byte(content))
	hash = fmt.Sprintf("%x", h.Sum(nil))
	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID: node, Kind: queue.KindMedia, Origin: queue.OriginUpload,
		SourcePath: "/card/" + name, LocalPath: local, FileName: name, FileExt: "jpg",
		SizeBytes: info.Size(), MtimeUnix: info.ModTime().Unix(), FullHash: hash, FastHash: "fast",
		UploadName: name, CaptureTimestamp: 1700000000, SourcePathHash: fmt.Sprintf("%x", sha256.Sum256([]byte(node))), LocalEditRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	return node, local, hash
}

// uploadServer is a stub branchDAM: handshake plus /upload, which echoes the
// body's BLAKE3 and places the node under 2026/2026-08-29/.
func uploadServer(t *testing.T, uploads *[]http.Header) *httptest.Server {
	return slowUploadServer(t, uploads, 0, nil)
}

// slowUploadServer is uploadServer whose /upload calls onUpload (if set) as
// soon as the request arrives and then takes delay to answer.
func slowUploadServer(t *testing.T, uploads *[]http.Header, delay time.Duration, onUpload func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agent/upload":
			if onUpload != nil {
				onUpload()
			}
			body, _ := io.ReadAll(r.Body)
			time.Sleep(delay)
			*uploads = append(*uploads, r.Header.Clone())
			h := blake3.New()
			_, _ = h.Write(body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodeUuid": "srv-node-1", "status": "UPLOADED", "bytesWritten": len(body),
				"blake3Hash": fmt.Sprintf("%x", h.Sum(nil)), "relativePath": "2026/2026-08-29/" + r.Header.Get("X-Filename"),
			})
		default:
			_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestQueueDrainerUploadsAQueuedDeferral: the tray's drainer (the real
// client, a stub server) uploads a queued deferral with no card in sight,
// carries the counts into the DrainSummary, and is not cut off when the pass
// context ends mid-upload -- nor does the closing bookkeeping trip over that
// spent context.
func TestQueueDrainerUploadsAQueuedDeferral(t *testing.T) {
	store := openTestQueueStore(t)
	node, local, hash := uploadQueueFixture(t, store, "queued bytes")
	var uploads []http.Header
	// The pass context ends the moment the upload request arrives (not on a
	// timer, so the quick phases before it cannot race a deadline): the
	// upload must carry on, and the closing bookkeeping must not use it.
	pass, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := slowUploadServer(t, &uploads, 100*time.Millisecond, cancel)

	d := &queueDrainer{client: branchdam.New(srv.URL, "test-key"), store: store, agentID: "agent-01", uploadCtx: context.Background()}

	summary, err := d.Drain(pass)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if summary.UploadsDone != 1 || summary.UploadsFailed != 0 || summary.Remaining != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(uploads) != 1 || uploads[0].Get("X-Blake3-Hash") != hash || uploads[0].Get("X-Filename") != "IMG_0001.JPG" {
		t.Fatalf("upload headers = %v", uploads)
	}
	r, ok, err := store.ByNodeUUID(context.Background(), node)
	if err != nil || !ok || r.UploadStatus != queue.StatusDone || r.ServerNodeUUID != "srv-node-1" {
		t.Fatalf("row = %+v ok=%v err=%v", r, ok, err)
	}
	if _, err := os.Stat(local); err == nil {
		t.Error("the local copy is still at its deferred name; it should have moved to the server's path")
	}
	if _, err := os.Stat(r.LocalPath); err != nil || !strings.HasSuffix(filepath.ToSlash(r.LocalPath), "2026/2026-08-29/IMG_0001.JPG") {
		t.Errorf("moved copy missing: path=%q err=%v", r.LocalPath, err)
	}
}

// TestQueueDrainerRechecksTheRunnerGateBeforeEachUpload: the tray's drainer
// is handed Runner.UploadGate, so a link that turns metered after the first
// upload holds the second (it stays PENDING, no attempt counted), and a
// failing probe holds every upload (fail closed).
func TestQueueDrainerRechecksTheRunnerGateBeforeEachUpload(t *testing.T) {
	store := openTestQueueStore(t)
	node1, _, _ := uploadQueueRow(t, store, "first bytes", "up-queued-1", "IMG_0001.JPG")
	node2, _, _ := uploadQueueRow(t, store, "second bytes", "up-queued-2", "IMG_0002.JPG")
	var uploads []http.Header
	srv := uploadServer(t, &uploads)

	runner := tray.NewRunner(nil, nil, t.TempDir())
	runner.SetPauseUploadOnMetered(true)
	d := &queueDrainer{client: branchdam.New(srv.URL, "test-key"), store: store, agentID: "agent-01", uploadGate: runner.UploadGate}

	runner.SetIsMeteredFunc(func() (bool, error) { return false, errors.New("no network manager") })
	if _, err := d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 0 {
		t.Fatalf("uploads with a failing probe = %d, want 0", len(uploads))
	}

	var calls int
	runner.SetIsMeteredFunc(func() (bool, error) { calls++; return calls > 1, nil })
	summary, err := d.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 || summary.UploadsDone != 1 || summary.Remaining != 1 {
		t.Fatalf("uploads = %d, summary = %+v, want 1 sent and 1 left", len(uploads), summary)
	}
	done, pending := 0, 0
	for _, n := range []string{node1, node2} {
		r, ok, err := store.ByNodeUUID(context.Background(), n)
		if err != nil || !ok {
			t.Fatalf("ByNodeUUID(%s): ok=%v err=%v", n, ok, err)
		}
		switch r.UploadStatus {
		case queue.StatusDone:
			done++
		case queue.StatusPending:
			pending++
			if r.UploadAttempts != 0 {
				t.Errorf("held row counted %d attempts", r.UploadAttempts)
			}
		}
	}
	if done != 1 || pending != 1 {
		t.Errorf("done = %d, pending = %d, want 1 and 1", done, pending)
	}
}

// TestRunDrainPassUploadGateHoldsQueuedUploads: queue-drain with
// pauseUploadOnMetered passes the metered gate, which holds the uploads.
func TestRunDrainPassUploadGateHoldsQueuedUploads(t *testing.T) {
	store := openTestQueueStore(t)
	node, _, _ := uploadQueueFixture(t, store, "queued bytes")
	var uploads []http.Header
	srv := uploadServer(t, &uploads)
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	if code := runDrainPass(&buf, client, store, "agent-01", 5*time.Second,
		ingest.WithUploadGate(func() (bool, error) { return true, nil })); code != 0 {
		t.Fatalf("exit code = %d\n%s", code, buf.String())
	}
	if len(uploads) != 0 {
		t.Fatalf("uploads on a metered link = %d, want 0", len(uploads))
	}
	if r, _, _ := store.ByNodeUUID(context.Background(), node); r.UploadStatus != queue.StatusPending {
		t.Errorf("row = %+v, want it still pending", r)
	}

	buf.Reset()
	if code := runDrainPass(&buf, client, store, "agent-01", 5*time.Second,
		ingest.WithUploadGate(func() (bool, error) { return false, nil })); code != 0 {
		t.Fatalf("exit code = %d\n%s", code, buf.String())
	}
	if len(uploads) != 1 || !strings.Contains(buf.String(), "queued uploads done: 1, permanently failed: 0") {
		t.Errorf("uploads = %d, output = %q", len(uploads), buf.String())
	}
}

// TestQueueCountsReaderReportsAFailedUploadAndRetriesIt: the tray's
// failed-rows list names the upload step with its reason, and a retry resets
// it.
func TestQueueCountsReaderReportsAFailedUploadAndRetriesIt(t *testing.T) {
	store := openTestQueueStore(t)
	node, _, _ := uploadQueueFixture(t, store, "queued bytes")
	ctx := context.Background()
	if err := store.MarkUploadFailed(ctx, node, "local copy changed since it was queued"); err != nil {
		t.Fatal(err)
	}
	reader := &queueCountsReader{store: store}

	rows, err := reader.FailedRows(ctx, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("FailedRows = %+v err = %v", rows, err)
	}
	if rows[0].Step != tray.FailedStepUpload || rows[0].LastError != "local copy changed since it was queued" || rows[0].NodeUUID != node {
		t.Errorf("row = %+v", rows[0])
	}

	counts, err := reader.RetryFailed(ctx, node)
	if err != nil || counts.Uploads != 1 || counts.ArchiveCopies != 0 || counts.Rebases != 0 {
		t.Fatalf("RetryFailed = %+v err = %v", counts, err)
	}
	if r, _, _ := store.ByNodeUUID(ctx, node); r.UploadStatus != queue.StatusPending {
		t.Errorf("row after retry = %+v", r)
	}
}

// TestPrintIngestReportQueuedUploadIsSafeToEject: a metered deferral that is
// recorded in queue.db no longer depends on the card, so the report says so
// and names the local copy rather than an archive path that does not exist.
func TestPrintIngestReportQueuedUploadIsSafeToEject(t *testing.T) {
	result := ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/card/a.jpg", Queued: true, UploadQueueID: "up-1", LocalPath: "/local/2026/a.jpg"},
	}}
	var buf bytes.Buffer
	if !printIngestReport(&buf, "/card", result, time.Second) {
		t.Fatalf("printIngestReport returned ok=false for a queued upload:\n%s", buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"[OK]   /card/a.jpg -> /local/2026/a.jpg (upload queued for queue-drain",
		"1 submitted, 0 skipped, 0 deferred, 0 failed",
		"safe to eject",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT safe to eject") || strings.Contains(out, "[DEFERRED]") {
		t.Errorf("a queued upload was reported as deferred:\n%s", out)
	}
}
