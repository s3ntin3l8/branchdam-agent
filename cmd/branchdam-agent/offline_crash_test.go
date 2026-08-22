package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// TestOfflineIngestCrashSafety is issue #4's central acceptance criterion,
// tested against a REAL killed process, not an in-process simulation: a
// child copy of this test binary (see TestHelperProcess below) runs
// `ingest -offline` against a fixture card and then calls os.Exit(1)
// immediately -- before any deferred cleanup, including queue.Store.Close --
// simulating the process dying the instant its queue.db commit returns.
// Separate OS process means a genuinely cold reopen of queue.db afterward:
// no shared Go heap, no warm in-process cache, nothing left over except
// what actually landed on disk. See internal/ingest/offline_test.go's
// TestIngestCardOfflineResumeAfterRestart for the in-process half of this
// property (same assertions, cheaper to run, not a substitute for this
// test).
//
// The server is unreachable throughout the crash phase (127.0.0.1:1, no
// listener), so the opportunistic inline EVENT_NODE_CREATED attempt is
// guaranteed to fail and leave the row PENDING -- that's what makes the
// resume-and-drain phase below a meaningful test of "retried correctly, not
// double-submitted under a new UUID" rather than a no-op.
func TestOfflineIngestCrashSafety(t *testing.T) {
	dir := t.TempDir()
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	queueDBPath := filepath.Join(dir, "queue.db")
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	srcContent := []byte("crash-safety-fixture-bytes")
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), srcContent, 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := fmt.Sprintf(`server:
  baseUrl: "http://127.0.0.1:1"
  apiKey: "0123456789abcdef0123456789abcdef"
agentId: "crash-test-agent"
pathMappings:
  - workstationPath: %q
    containerPath: "/storage/archive"
ingest:
  archiveRoot: %q
  localEditRoot: %q
  pathTemplate: "{original_name}"
offline:
  queueDbPath: %q
  tier0ContainerRoot: "/storage/staging/crash-test-agent"
`, archiveRoot, archiveRoot, localRoot, queueDBPath)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		"BRANCHDAM_AGENT_WANT_HELPER_PROCESS=1",
		"BRANCHDAM_AGENT_HELPER_CONFIG="+cfgPath,
		"BRANCHDAM_AGENT_HELPER_CARD="+cardRoot,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// TestHelperProcess always os.Exit(1) itself once it reaches that point
	// -- an *exec.ExitError with code 1 is the expected, successful outcome
	// of the crash simulation, not a test failure.
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("helper process: expected an *exec.ExitError (exit 1), got %v\nstdout:\n%s\nstderr:\n%s", runErr, stdout.String(), stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("helper process exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", exitErr.ExitCode(), stdout.String(), stderr.String())
	}

	// --- Restart: open a fresh Store over the same file, cold. ---
	store, err := queue.Open(queueDBPath)
	if err != nil {
		t.Fatalf("reopen queue.db after crash: %v", err)
	}
	defer func() { _ = store.Close() }()

	all, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d queue rows after the crash, want exactly 1 (nothing lost, nothing duplicated)", len(all))
	}
	rec := all[0]
	if rec.NodeUUID == "" {
		t.Fatal("expected a non-empty NodeUUID to have survived the crash")
	}
	if rec.NodeCreatedStatus != queue.StatusPending {
		t.Errorf("NodeCreatedStatus = %q, want PENDING (the opportunistic inline POST to an unreachable server must have failed and left this retryable)", rec.NodeCreatedStatus)
	}

	gotContent, err := os.ReadFile(rec.LocalPath)
	if err != nil {
		t.Fatalf("local copy did not survive the crash: %v", err)
	}
	if !bytes.Equal(gotContent, srcContent) {
		t.Error("local copy content does not match the source -- corrupted or incomplete across the crash")
	}
	if got := blake3Hex(gotContent); got != rec.FullHash {
		t.Errorf("local copy hash %s does not match the queued row's FullHash %s", got, rec.FullHash)
	}

	// --- Resume: ingest the SAME card again, in-process, against the
	// restarted queue.db. Must recognize the existing row, must NOT re-mint
	// NodeUUID, must NOT duplicate the row. ---
	client := branchdam.New("http://127.0.0.1:1", "0123456789abcdef0123456789abcdef")
	engine := ingest.NewEngine(client, "crash-test-agent",
		config.IngestConfig{ArchiveRoot: archiveRoot, LocalEditRoot: localRoot, PathTemplate: "{original_name}"},
		[]config.PathMapping{{WorkstationPath: archiveRoot, ContainerPath: "/storage/archive"}},
	)
	engine.Queue = store
	engine.Tier0ContainerRoot = "/storage/staging/crash-test-agent"

	res, err := engine.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files on resume, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if !fr.AlreadyQueued {
		t.Error("expected the resumed ingest to recognize the existing queue row rather than starting over")
	}
	if fr.NodeUUID != rec.NodeUUID {
		t.Errorf("resumed NodeUUID = %q, want %q (must never re-mint across a restart)", fr.NodeUUID, rec.NodeUUID)
	}

	allAfterResume, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(allAfterResume) != 1 {
		t.Fatalf("got %d rows after resume, want exactly 1 (no duplicate row from the resumed ingest)", len(allAfterResume))
	}

	// --- Drain against a real HTTP server: the row queued before the crash
	// must be submitted exactly once, using the SAME NodeUUID that survived
	// the crash. ---
	var receivedUUIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/handshake", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"test","serverTimeUnix":0,"pendingEventsCount":0}`))
	})
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			AgentID   string `json:"agentId"`
			EventType string `json:"eventType"`
			Payload   string `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		var payload struct {
			NodeUUID string `json:"nodeUuid"`
		}
		_ = json.Unmarshal([]byte(env.Payload), &payload)
		receivedUUIDs = append(receivedUUIDs, payload.NodeUUID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	drainClient := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	stats, err := ingest.Drain(context.Background(), drainClient, store, "crash-test-agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeCreatedSent != 1 {
		t.Errorf("NodeCreatedSent = %d, want 1", stats.NodeCreatedSent)
	}
	if len(receivedUUIDs) != 1 || receivedUUIDs[0] != rec.NodeUUID {
		t.Errorf("server received UUIDs %v, want exactly [%s] -- a crash must never cause a resubmission under a new UUID", receivedUUIDs, rec.NodeUUID)
	}

	recAfterDrain, ok, err := store.ByNodeUUID(context.Background(), rec.NodeUUID)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if recAfterDrain.NodeCreatedStatus != queue.StatusSubmitted {
		t.Errorf("NodeCreatedStatus after drain = %q, want SUBMITTED", recAfterDrain.NodeCreatedStatus)
	}

	// A second drain pass must not resubmit an already-SUBMITTED row.
	if _, err := ingest.Drain(context.Background(), drainClient, store, "crash-test-agent", nil); err != nil {
		t.Fatal(err)
	}
	if len(receivedUUIDs) != 1 {
		t.Errorf("a second drain pass resubmitted node_created: got %d total POSTs, want 1", len(receivedUUIDs))
	}
}

func blake3Hex(data []byte) string {
	h := blake3.New()
	_, _ = h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// TestHelperProcess is not a real test on its own -- it is the re-exec
// target TestOfflineIngestCrashSafety spawns as a separate OS process,
// following the standard Go idiom for testing real-process-death behavior
// (the same pattern os/exec's own test suite uses). It does nothing unless
// BRANCHDAM_AGENT_WANT_HELPER_PROCESS=1 is set (so a normal `go test` run
// never executes the exit-1 branch), in which case it runs
// `ingest -offline` for the configured card/config and then calls
// os.Exit(1) UNCONDITIONALLY immediately after -- deliberately skipping any
// deferred cleanup (queue.Store.Close, etc.) a graceful shutdown would run,
// simulating the process being killed the instant after its queue.db INSERT
// commits.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("BRANCHDAM_AGENT_WANT_HELPER_PROCESS") != "1" {
		return
	}
	cfgPath := os.Getenv("BRANCHDAM_AGENT_HELPER_CONFIG")
	cardPath := os.Getenv("BRANCHDAM_AGENT_HELPER_CARD")
	_ = run([]string{"ingest", "-config", cfgPath, "-card", cardPath, "-offline", "-timeout", "20s"})
	os.Exit(1)
}
