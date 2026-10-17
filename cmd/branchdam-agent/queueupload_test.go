package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	root := t.TempDir()
	local = filepath.Join(root, "2026", "IMG_0001.JPG")
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
	node = "up-queued-1"
	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID: node, Kind: queue.KindMedia, Origin: queue.OriginUpload,
		SourcePath: "/card/IMG_0001.JPG", LocalPath: local, FileName: "IMG_0001.JPG", FileExt: "jpg",
		SizeBytes: info.Size(), MtimeUnix: info.ModTime().Unix(), FullHash: hash, FastHash: "fast",
		UploadName: "IMG_0001.JPG", CaptureTimestamp: 1700000000, SourcePathHash: strings.Repeat("ab", 32), LocalEditRoot: root,
	}); err != nil {
		t.Fatal(err)
	}
	return node, local, hash
}

// uploadServer is a stub branchDAM: handshake plus /upload, which echoes the
// body's BLAKE3 and places the node under 2026/2026-08-29/.
func uploadServer(t *testing.T, uploads *[]http.Header) *httptest.Server {
	return slowUploadServer(t, uploads, 0)
}

// slowUploadServer is uploadServer whose /upload takes delay to answer.
func slowUploadServer(t *testing.T, uploads *[]http.Header, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agent/upload":
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
// context's deadline expires mid-upload -- nor does the closing bookkeeping
// trip over that spent deadline.
func TestQueueDrainerUploadsAQueuedDeferral(t *testing.T) {
	store := openTestQueueStore(t)
	node, local, hash := uploadQueueFixture(t, store, "queued bytes")
	var uploads []http.Header
	// The upload takes longer than the pass context's deadline: the quick
	// phases finish inside it, then it expires while the upload streams.
	srv := slowUploadServer(t, &uploads, 400*time.Millisecond)

	d := &queueDrainer{client: branchdam.New(srv.URL, "test-key"), store: store, agentID: "agent-01", uploadCtx: context.Background()}
	pass, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

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
