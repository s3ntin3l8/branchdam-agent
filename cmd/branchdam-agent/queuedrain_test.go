package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
