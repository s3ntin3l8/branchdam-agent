package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// fakeDrainClient is a scriptable drainClient: each method consults a
// caller-supplied function so a test can simulate any combination of
// success/transient-failure/fatal-failure across the three calls Drain
// makes.
type fakeDrainClient struct {
	handshakeFn func() (*branchdam.HandshakeResponse, error)
	nodeCreated func(payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error)
	rebase      func(req branchdam.RebaseRequest) (*branchdam.RebaseResponse, error)

	nodeCreatedCalls []branchdam.NodeCreatedPayload
	rebaseCalls      []branchdam.RebaseRequest
}

func (f *fakeDrainClient) Handshake(context.Context, branchdam.HandshakeRequest) (*branchdam.HandshakeResponse, error) {
	if f.handshakeFn != nil {
		return f.handshakeFn()
	}
	return &branchdam.HandshakeResponse{OK: true, ServerVersion: "test"}, nil
}

func (f *fakeDrainClient) PostNodeCreated(_ context.Context, _ string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
	f.nodeCreatedCalls = append(f.nodeCreatedCalls, payload)
	if f.nodeCreated != nil {
		return f.nodeCreated(payload)
	}
	return &branchdam.EventResponse{EventID: "evt-" + payload.NodeUUID}, nil
}

func (f *fakeDrainClient) Rebase(_ context.Context, req branchdam.RebaseRequest) (*branchdam.RebaseResponse, error) {
	f.rebaseCalls = append(f.rebaseCalls, req)
	if f.rebase != nil {
		return f.rebase(req)
	}
	return &branchdam.RebaseResponse{NodeUUID: req.NodeUUID, Status: "REBASED"}, nil
}

// seedMediaRow writes localContent to a fresh local file, inserts a MEDIA
// queue row pointing at it (and at a not-yet-existing archivePath), and
// returns the row's NodeUUID plus its full hash. Bypasses
// Engine.ingestFileOffline entirely so Drain's own behavior can be tested
// in isolation from the ingest-time code path.
func seedMediaRow(t *testing.T, store *queue.Store, dir, nodeUUID string, localContent []byte) (fullHash string) {
	t.Helper()
	localPath := filepath.Join(dir, "local", nodeUUID+".bin")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, localContent, 0o644); err != nil {
		t.Fatal(err)
	}
	fullHash = blake3Hex(t, localPath)
	archivePath := filepath.Join(dir, "archive", nodeUUID+".bin")

	payload := branchdam.NodeCreatedPayload{
		NodeUUID: nodeUUID,
		FilePath: "/storage/staging/agent-1/" + nodeUUID + ".bin",
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	rec := queue.NewRecord{
		NodeUUID:               nodeUUID,
		Kind:                   queue.KindMedia,
		SourcePath:             "/card/" + nodeUUID + ".bin",
		LocalPath:              localPath,
		ArchivePath:            archivePath,
		ArchiveContainerPath:   "/storage/archive/" + nodeUUID + ".bin",
		Tier0ContainerPath:     payload.FilePath,
		FileName:               nodeUUID + ".bin",
		FileExt:                "bin",
		SizeBytes:              int64(len(localContent)),
		MtimeUnix:              1700000000,
		FullHash:               fullHash,
		FastHash:               "0123456789abcdef",
		NodeCreatedPayloadJSON: string(payloadJSON),
	}
	if err := store.InsertPending(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return fullHash
}

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestDrainSubmitsNodeCreatedAndCopiesArchiveButWithholdsRebaseUntilDwell(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-dwell", []byte("hello"))

	client := &fakeDrainClient{}
	t0 := time.Unix(1_800_000_000, 0)

	stats, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0))
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeCreatedSent != 1 {
		t.Errorf("NodeCreatedSent = %d, want 1", stats.NodeCreatedSent)
	}
	if stats.ArchiveCopiesDone != 1 {
		t.Errorf("ArchiveCopiesDone = %d, want 1", stats.ArchiveCopiesDone)
	}
	if stats.RebasesDone != 0 {
		t.Errorf("RebasesDone = %d, want 0 (dwell not yet elapsed)", stats.RebasesDone)
	}
	if len(client.rebaseCalls) != 0 {
		t.Errorf("expected zero Rebase calls before MinRebaseDwell elapses, got %d", len(client.rebaseCalls))
	}
	if stats.Remaining != 1 {
		t.Errorf("Remaining = %d, want 1", stats.Remaining)
	}

	// A second pass immediately after (dwell still not elapsed) must not
	// call Rebase, and must not resubmit an already-SUBMITTED node_created.
	stats2, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if stats2.NodeCreatedSent != 0 {
		t.Errorf("second pass NodeCreatedSent = %d, want 0 (already SUBMITTED)", stats2.NodeCreatedSent)
	}
	if len(client.nodeCreatedCalls) != 1 {
		t.Errorf("expected exactly one PostNodeCreated call total, got %d", len(client.nodeCreatedCalls))
	}
	if len(client.rebaseCalls) != 0 {
		t.Errorf("still expected zero Rebase calls, got %d", len(client.rebaseCalls))
	}

	// A third pass after MinRebaseDwell has elapsed must call Rebase and
	// complete the row.
	stats3, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(MinRebaseDwell+time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if stats3.RebasesDone != 1 {
		t.Errorf("RebasesDone = %d, want 1", stats3.RebasesDone)
	}
	if stats3.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", stats3.Remaining)
	}
	if len(client.rebaseCalls) != 1 {
		t.Fatalf("expected exactly one Rebase call, got %d", len(client.rebaseCalls))
	}
	if client.rebaseCalls[0].TargetPath != "/storage/archive/uuid-dwell.bin" {
		t.Errorf("Rebase targetPath = %q, want the final Tier-3 container path", client.rebaseCalls[0].TargetPath)
	}
}

func TestDrainClassifiesFatalRebaseErrorAsTerminal(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-archived", []byte("hello"))

	client := &fakeDrainClient{
		rebase: func(branchdam.RebaseRequest) (*branchdam.RebaseResponse, error) {
			return nil, &branchdam.HTTPError{StatusCode: 400, Body: "node is archived, refusing to rebase"}
		},
	}
	t0 := time.Unix(1_800_000_000, 0)

	if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0)); err != nil {
		t.Fatal(err)
	}
	stats, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(MinRebaseDwell+time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if stats.RebasesFailed != 1 {
		t.Errorf("RebasesFailed = %d, want 1", stats.RebasesFailed)
	}

	rec, ok, err := store.ByNodeUUID(context.Background(), "uuid-archived")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rec.RebaseStatus != queue.StatusFailed {
		t.Errorf("RebaseStatus = %q, want FAILED", rec.RebaseStatus)
	}

	// A further pass, well past the dwell, must not retry a FAILED row.
	rebaseCallsBefore := len(client.rebaseCalls)
	if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if len(client.rebaseCalls) != rebaseCallsBefore {
		t.Error("expected no further Rebase attempts on an already-FAILED row")
	}
}

func TestDrainRetriesTransientRebaseError(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-transient", []byte("hello"))

	attempt := 0
	client := &fakeDrainClient{
		rebase: func(branchdam.RebaseRequest) (*branchdam.RebaseResponse, error) {
			attempt++
			if attempt == 1 {
				return nil, &branchdam.HTTPError{StatusCode: 500, Body: "internal server error"}
			}
			return &branchdam.RebaseResponse{Status: "REBASED"}, nil
		},
	}
	t0 := time.Unix(1_800_000_000, 0)
	if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0)); err != nil {
		t.Fatal(err)
	}

	afterDwell := t0.Add(MinRebaseDwell + time.Second)
	stats, err := Drain(context.Background(), client, store, "agent-1", fixedNow(afterDwell))
	if err != nil {
		t.Fatal(err)
	}
	if stats.RebasesDone != 0 || stats.RebasesFailed != 0 {
		t.Errorf("expected transient failure to leave row retryable, got %+v", stats)
	}
	rec, _, err := store.ByNodeUUID(context.Background(), "uuid-transient")
	if err != nil {
		t.Fatal(err)
	}
	if rec.RebaseStatus != queue.StatusPending {
		t.Errorf("RebaseStatus = %q, want PENDING after a transient failure", rec.RebaseStatus)
	}
	if rec.RebaseAttempts != 1 {
		t.Errorf("RebaseAttempts = %d, want 1", rec.RebaseAttempts)
	}

	// Immediately retrying must respect the backoff and NOT call Rebase
	// again yet.
	callsBefore := len(client.rebaseCalls)
	if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(afterDwell.Add(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if len(client.rebaseCalls) != callsBefore {
		t.Error("expected backoff to withhold the retry immediately after a transient failure")
	}

	// After the backoff window, the retry succeeds.
	stats2, err := Drain(context.Background(), client, store, "agent-1", fixedNow(afterDwell.Add(backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	if stats2.RebasesDone != 1 {
		t.Errorf("RebasesDone = %d, want 1 after backoff elapses", stats2.RebasesDone)
	}
}

func TestDrainNodeCreatedTransientFailureRetriesWithBackoff(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-nc-retry", []byte("hello"))

	fail := true
	client := &fakeDrainClient{
		nodeCreated: func(branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
			if fail {
				return nil, fmt.Errorf("connection refused")
			}
			return &branchdam.EventResponse{EventID: "evt-later"}, nil
		},
	}
	t0 := time.Unix(1_800_000_000, 0)
	stats, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0))
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeCreatedSent != 0 {
		t.Errorf("NodeCreatedSent = %d, want 0", stats.NodeCreatedSent)
	}
	rec, _, err := store.ByNodeUUID(context.Background(), "uuid-nc-retry")
	if err != nil {
		t.Fatal(err)
	}
	if rec.NodeCreatedAttempts != 1 || rec.NodeCreatedStatus != queue.StatusPending {
		t.Errorf("unexpected state after a failed attempt: %+v", rec)
	}

	// Immediately retrying is withheld by backoff.
	if _, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if len(client.nodeCreatedCalls) != 1 {
		t.Error("expected backoff to withhold an immediate retry")
	}

	fail = false
	stats2, err := Drain(context.Background(), client, store, "agent-1", fixedNow(t0.Add(backoffCap)))
	if err != nil {
		t.Fatal(err)
	}
	if stats2.NodeCreatedSent != 1 {
		t.Errorf("NodeCreatedSent = %d, want 1 once the backoff window elapses and the call succeeds", stats2.NodeCreatedSent)
	}
}

func TestDrainSidecarRowNeverReachesRebasePhase(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	ctx := context.Background()

	localPath := filepath.Join(dir, "local", "sidecar.xmp")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("<xmp/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	fullHash := blake3Hex(t, localPath)

	rec := queue.NewRecord{
		NodeUUID:             "uuid-sidecar-drain",
		Kind:                 queue.KindSidecar,
		SourcePath:           "/card/sidecar.xmp",
		LocalPath:            localPath,
		ArchivePath:          filepath.Join(dir, "archive", "sidecar.xmp"),
		ArchiveContainerPath: "/storage/archive/sidecar.xmp",
		FileName:             "sidecar.xmp",
		FileExt:              "xmp",
		SizeBytes:            6,
		MtimeUnix:            1700000000,
		FullHash:             fullHash,
		FastHash:             "0123456789abcdef",
	}
	if err := store.InsertPending(ctx, rec); err != nil {
		t.Fatal(err)
	}

	client := &fakeDrainClient{}
	t0 := time.Unix(1_800_000_000, 0)
	stats, err := Drain(ctx, client, store, "agent-1", fixedNow(t0.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchiveCopiesDone != 1 {
		t.Errorf("ArchiveCopiesDone = %d, want 1", stats.ArchiveCopiesDone)
	}
	if len(client.nodeCreatedCalls) != 0 || len(client.rebaseCalls) != 0 {
		t.Errorf("a SIDECAR row must never trigger node_created or rebase: nodeCreated=%d rebase=%d", len(client.nodeCreatedCalls), len(client.rebaseCalls))
	}
	if stats.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0 (archive_copy DONE is sufficient for a SIDECAR row)", stats.Remaining)
	}
}

func TestDrainHandshakeFailureDoesNotAbortPass(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	seedMediaRow(t, store, dir, "uuid-hs", []byte("hello"))

	client := &fakeDrainClient{
		handshakeFn: func() (*branchdam.HandshakeResponse, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	stats, err := Drain(context.Background(), client, store, "agent-1", fixedNow(time.Unix(1_800_000_000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if stats.HandshakeErr == nil {
		t.Error("expected HandshakeErr to be set")
	}
	if stats.NodeCreatedSent != 1 || stats.ArchiveCopiesDone != 1 {
		t.Errorf("expected the rest of the pass to proceed despite a handshake failure, got %+v", stats)
	}
}
