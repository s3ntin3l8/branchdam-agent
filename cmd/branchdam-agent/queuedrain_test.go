package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

func openTestQueueStore(t *testing.T) *queue.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := queue.Open(filepath.Join(dir, "queue.db"))
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRunDrainPassEmptyQueueHandshakeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)

	var buf bytes.Buffer
	code := runDrainPass(&buf, client, store, "agent-01", 5*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	out := buf.String()
	if !strings.Contains(out, "handshake: ok, server version 0.5.0") {
		t.Errorf("output missing successful handshake line: %q", out)
	}
	if !strings.Contains(out, "node_created submitted: 0") {
		t.Errorf("output missing zero-row summary: %q", out)
	}
}

func TestRunDrainPassHandshakeUnreachableIsLogOnly(t *testing.T) {
	// An unreachable server must not fail the whole pass -- Drain treats
	// handshake failure as log-only (plan contract gap 2: handshake is not
	// a resume mechanism, and a drain pass with nothing else pending should
	// still report success).
	client := branchdam.New("http://127.0.0.1:1", "test-key")
	store := openTestQueueStore(t)

	var buf bytes.Buffer
	code := runDrainPass(&buf, client, store, "agent-01", 2*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (handshake failure alone must not fail the pass)", code)
	}
	out := buf.String()
	if !strings.Contains(out, "handshake: unreachable") {
		t.Errorf("output missing unreachable-handshake line: %q", out)
	}
}

func TestRunDrainPassSubmitsPendingNodeCreated(t *testing.T) {
	var gotAgentID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/handshake"):
			_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
		case strings.HasSuffix(r.URL.Path, "/events"):
			var env branchdam.EventEnvelope
			_ = json.NewDecoder(r.Body).Decode(&env)
			gotAgentID = env.AgentID
			_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)

	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID:               "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		Kind:                   queue.KindMedia,
		SourcePath:             "/Volumes/CARD/f.jpg",
		LocalPath:              "/tmp/local/f.jpg",
		ArchivePath:            "/tmp/archive/f.jpg",
		ArchiveContainerPath:   "/storage/archive/f.jpg",
		Tier0ContainerPath:     "/storage/staging/agent-01/f.jpg",
		FileName:               "f.jpg",
		FileExt:                ".jpg",
		SizeBytes:              10,
		MtimeUnix:              1,
		FullHash:               "def",
		FastHash:               "abc",
		NodeCreatedPayloadJSON: `{"nodeUuid":"0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60","filePath":"/storage/staging/agent-01/f.jpg"}`,
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	var buf bytes.Buffer
	code := runDrainPass(&buf, client, store, "agent-01", 5*time.Second)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if gotAgentID != "agent-01" {
		t.Errorf("agentId sent to server = %q, want agent-01", gotAgentID)
	}
	if !strings.Contains(buf.String(), "node_created submitted: 1") {
		t.Errorf("output missing submitted-1 summary: %q", buf.String())
	}
}

// TestRunQueueDrainCmdRetryFailed: -retry-failed puts a FAILED rebase and a
// FAILED archive copy back to pending before the pass, so the same
// invocation rebases the one and copies the other; the default invocation
// leaves both FAILED.
func TestRunQueueDrainCmdRetryFailed(t *testing.T) {
	var rebaseCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/handshake"):
			_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
		case strings.HasSuffix(r.URL.Path, "/rebase"):
			rebaseCalls++
			_, _ = w.Write([]byte(`{"id":1,"nodeUuid":"0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f61","storageLocationId":3,"filePath":"/storage/archive/f.jpg","status":"REBASED"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "queue.db")
	const nodeUUID = "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f61"
	ctx := context.Background()
	store, err := queue.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPending(ctx, queue.NewRecord{
		NodeUUID:               nodeUUID,
		Kind:                   queue.KindMedia,
		SourcePath:             "/Volumes/CARD/f.jpg",
		LocalPath:              "/tmp/local/f.jpg",
		ArchivePath:            "/tmp/archive/f.jpg",
		ArchiveContainerPath:   "/storage/archive/f.jpg",
		Tier0ContainerPath:     "/storage/staging/agent-01/f.jpg",
		FileName:               "f.jpg",
		FileExt:                ".jpg",
		SizeBytes:              10,
		MtimeUnix:              1,
		FullHash:               strings.Repeat("d", 64),
		FastHash:               "abc",
		NodeCreatedPayloadJSON: `{"nodeUuid":"` + nodeUUID + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkNodeCreatedSubmitted(ctx, nodeUUID, "evt-1", time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchiveCopyDone(ctx, nodeUUID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRebaseFailed(ctx, nodeUUID, "branchdam: server returned HTTP 401"); err != nil {
		t.Fatal(err)
	}

	// A second row whose archive copy FAILED (a different file occupied
	// the archive path); the operator has since moved it aside.
	const copyUUID = "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f62"
	localPath := filepath.Join(dir, "local", "g.jpg")
	archivePath := filepath.Join(dir, "archive", "g.jpg")
	content := []byte("g-bytes")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, localPath, string(content))
	sum := blake3.Sum256(content)
	if err := store.InsertPending(ctx, queue.NewRecord{
		NodeUUID:               copyUUID,
		Kind:                   queue.KindMedia,
		SourcePath:             "/Volumes/CARD/g.jpg",
		LocalPath:              localPath,
		ArchivePath:            archivePath,
		ArchiveContainerPath:   "/storage/archive/g.jpg",
		Tier0ContainerPath:     "/storage/staging/agent-01/g.jpg",
		FileName:               "g.jpg",
		FileExt:                ".jpg",
		SizeBytes:              int64(len(content)),
		MtimeUnix:              1,
		FullHash:               hex.EncodeToString(sum[:]),
		FastHash:               "abc",
		NodeCreatedPayloadJSON: `{"nodeUuid":"` + copyUUID + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkNodeCreatedSubmitted(ctx, copyUUID, "evt-2", time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchiveCopyFailed(ctx, copyUUID, "archive destination exists with different content"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`server:
  baseUrl: %q
  apiKey: "0123456789abcdef0123456789abcdef"
agentId: "agent-01"
offline:
  queueDbPath: %q
`, srv.URL, dbPath))

	record := func(uuid string) queue.Record {
		t.Helper()
		s, err := queue.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		rec, _, err := s.ByNodeUUID(ctx, uuid)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	rebaseStatus := func() string { return record(nodeUUID).RebaseStatus }

	if code := runQueueDrainCmd([]string{"-config", cfgPath, "-timeout", "5s"}); code != 0 {
		t.Fatalf("plain pass exit code = %d, want 0", code)
	}
	if rebaseCalls != 0 || rebaseStatus() != queue.StatusFailed {
		t.Fatalf("plain pass: rebase calls=%d status=%q, want 0 and FAILED left alone", rebaseCalls, rebaseStatus())
	}
	if got := record(copyUUID).ArchiveCopyStatus; got != queue.StatusFailed {
		t.Fatalf("plain pass: ArchiveCopyStatus = %q, want FAILED left alone", got)
	}

	if code := runQueueDrainCmd([]string{"-config", cfgPath, "-timeout", "5s", "-retry-failed"}); code != 0 {
		t.Fatalf("-retry-failed exit code = %d, want 0", code)
	}
	// Both rows rebase in that pass: the failed rebase, and the reset
	// archive copy once it lands (its node_created dwell is long past).
	if rebaseCalls != 2 {
		t.Errorf("rebase calls = %d, want 2", rebaseCalls)
	}
	if got := rebaseStatus(); got != queue.StatusDone {
		t.Errorf("RebaseStatus = %q, want DONE after -retry-failed", got)
	}
	if got := record(copyUUID); got.ArchiveCopyStatus != queue.StatusDone || got.RebaseStatus != queue.StatusDone {
		t.Errorf("copy row after -retry-failed: archive_copy=%q rebase=%q, want DONE/DONE", got.ArchiveCopyStatus, got.RebaseStatus)
	}
	if got, err := os.ReadFile(archivePath); err != nil || string(got) != string(content) {
		t.Errorf("archive copy = %q, %v; want the row's bytes", got, err)
	}
}

func TestResetFailedReportsCounts(t *testing.T) {
	store := openTestQueueStore(t)
	var buf bytes.Buffer
	if code := resetFailed(&buf, store, 5*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "reset 0 failed archive copies, 0 failed rebases and 0 failed uploads to pending") {
		t.Errorf("output = %q", buf.String())
	}
	_ = store.Close()
	if code := resetFailed(&buf, store, 5*time.Second); code != 1 {
		t.Errorf("exit code on closed store = %d, want 1", code)
	}
}

func TestRunQueueDrainCmdMissingQueueDBPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, `server:
  baseUrl: "http://127.0.0.1:1"
  apiKey: "0123456789abcdef0123456789abcdef"
agentId: "agent-01"
`)
	code := runQueueDrainCmd([]string{"-config", cfgPath})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (missing offline.queueDbPath must be caught before opening anything)", code)
	}
}

func TestRunQueueDrainCmdMissingAPIKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`server:
  baseUrl: "http://127.0.0.1:1"
agentId: "agent-01"
offline:
  queueDbPath: %q
`, filepath.Join(dir, "queue.db")))
	code := runQueueDrainCmd([]string{"-config", cfgPath})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (missing server.apiKey must be caught before opening anything)", code)
	}
}

func TestRunQueueDrainCmdBadConfigPath(t *testing.T) {
	code := runQueueDrainCmd([]string{"-config", filepath.Join(t.TempDir(), "does-not-exist.yaml")})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunQueueDrainCmdBadFlag(t *testing.T) {
	code := runQueueDrainCmd([]string{"-not-a-real-flag"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (flag parse error)", code)
	}
}

func TestRunQueueDrainCmdSinglePassSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`server:
  baseUrl: %q
  apiKey: "0123456789abcdef0123456789abcdef"
agentId: "agent-01"
offline:
  queueDbPath: %q
`, srv.URL, filepath.Join(dir, "queue.db")))

	code := runQueueDrainCmd([]string{"-config", cfgPath, "-timeout", "5s"})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
