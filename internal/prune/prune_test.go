package prune

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

func openTestStore(t *testing.T) *queue.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := queue.Open(filepath.Join(dir, "queue.db"))
	if err != nil {
		t.Fatalf("queue.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// nodeStatusServer answers POST /api/v1/agent/node-status from a fixed
// map of NodeUUID -> entry, and records every UUID it was ever asked about
// (so a test can assert a UUID was -- or, for the sidecar-skip case, was
// not -- looked up at all).
func nodeStatusServer(t *testing.T, byUUID map[string]branchdam.NodeStatusEntry) (*httptest.Server, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req branchdam.NodeStatusRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		asked = append(asked, req.NodeUUIDs...)
		resp := branchdam.NodeStatusResponse{}
		for _, u := range req.NodeUUIDs {
			if e, ok := byUUID[u]; ok {
				resp.Statuses = append(resp.Statuses, e)
			} else {
				resp.Statuses = append(resp.Statuses, branchdam.NodeStatusEntry{NodeUUID: u, Found: false})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// pruneTestFixture bundles the config, store, and LocalEditRoot a prune test
// needs, following openTestStore's t.TempDir()-per-test convention.
func pruneTestFixture(t *testing.T) (config.Config, *queue.Store, string) {
	t.Helper()
	dir := t.TempDir()
	localRoot := filepath.Join(dir, "local")
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t)
	cfg := config.Config{
		Ingest: config.IngestConfig{LocalEditRoot: localRoot},
	}
	return cfg, store, localRoot
}

// insertDoneRow writes a queue.db row and immediately marks its
// archive_copy and rebase steps DONE (i.e. Record.Done() == true), the
// precondition every prune candidate must satisfy before it's even looked
// at. Also writes a real file at localPath with the given content, and sets
// the row's SizeBytes/MtimeUnix to match that file's actual on-disk stat
// (mirroring ingestFileOffline's own os.Chtimes-then-record-mtime
// contract) unless overridden by ageHours.
func insertDoneRow(t *testing.T, store *queue.Store, nodeUUID, localPath, content string, ageHours int, kind string) {
	t.Helper()
	if err := os.WriteFile(localPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	if err := os.Chtimes(localPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID:             nodeUUID,
		Kind:                 kind,
		SourcePath:           "/Volumes/CARD/" + filepath.Base(localPath),
		LocalPath:            localPath,
		ArchivePath:          "/tmp/archive/" + filepath.Base(localPath),
		ArchiveContainerPath: "/storage/archive/" + filepath.Base(localPath),
		Tier0ContainerPath:   "/storage/staging/agent-01/" + filepath.Base(localPath),
		FileName:             filepath.Base(localPath),
		FileExt:              filepath.Ext(localPath),
		SizeBytes:            info.Size(),
		MtimeUnix:            info.ModTime().Unix(),
		FullHash:             strings.Repeat("ab", 32),
		FastHash:             "abc",
	}); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if kind == queue.KindMedia {
		if err := store.MarkNodeCreatedSubmitted(context.Background(), nodeUUID, "evt-1", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkArchiveCopyDone(context.Background(), nodeUUID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRebaseDone(context.Background(), nodeUUID); err != nil {
		t.Fatal(err)
	}
}

func verifiedEntry(uuid string) branchdam.NodeStatusEntry {
	return branchdam.NodeStatusEntry{
		NodeUUID: uuid, Found: true, LifecycleState: "ACTIVE", Tier: "TIER3_MASTER_ARCHIVE", Verified: true,
	}
}

func TestPassDeletesVerifiedAgedFile(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000001"
	path := filepath.Join(root, "clip.mov")
	insertDoneRow(t, store, uuid, path, "verified-content", 48, queue.KindMedia)

	srv, asked := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1 (%+v)", stats.Pruned, stats)
	}
	if len(*asked) != 1 || (*asked)[0] != uuid {
		t.Errorf("asked = %v, want exactly [%s]", *asked, uuid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be deleted, stat err = %v", path, err)
	}
}

func TestPassSkipsNotYetDone(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000002"
	path := filepath.Join(root, "pending.mov")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(path, mtime, mtime)
	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID: uuid, Kind: queue.KindMedia, SourcePath: "/c/pending.mov", LocalPath: path,
		ArchivePath: "/tmp/a/pending.mov", ArchiveContainerPath: "/storage/archive/pending.mov",
		FileName: "pending.mov", FileExt: ".mov", SizeBytes: 1, MtimeUnix: mtime.Unix(),
		FullHash: strings.Repeat("cd", 32), FastHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately NOT marking archive_copy/rebase done -- Record.Done() is false.

	srv, asked := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Evaluated != 0 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want a not-Done() row never evaluated", stats)
	}
	if len(*asked) != 0 {
		t.Errorf("asked = %v, want no node-status call for a not-Done() row", *asked)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s untouched: %v", path, err)
	}
}

func TestPassSkipsTooYoung(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000003"
	path := filepath.Join(root, "fresh.mov")
	insertDoneRow(t, store, uuid, path, "fresh-content", 1, queue.KindMedia) // only 1h old, under the 24h floor

	srv, asked := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedTooYoung != 1 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want SkippedTooYoung=1 Pruned=0", stats)
	}
	if len(*asked) != 0 {
		t.Errorf("asked = %v, want no node-status call for a too-young row", *asked)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s untouched: %v", path, err)
	}
}

func TestPassSkipsAlreadyGone(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000004"
	path := filepath.Join(root, "gone.mov")
	insertDoneRow(t, store, uuid, path, "will-be-removed", 48, queue.KindMedia)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	srv, asked := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedAlreadyGone != 1 || stats.Errors != 0 {
		t.Errorf("stats = %+v, want SkippedAlreadyGone=1 Errors=0 (a vanished file is not an error)", stats)
	}
	if len(*asked) != 0 {
		t.Errorf("asked = %v, want no node-status call once the file's already gone", *asked)
	}
}

func TestPassSkipsFileChangedSinceQueued(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000005"
	path := filepath.Join(root, "changed.mov")
	insertDoneRow(t, store, uuid, path, "original-content", 48, queue.KindMedia)

	// Mutate the file after it was queued -- size and mtime both drift from
	// what the row recorded.
	mtime := time.Now().Add(-48 * time.Hour)
	if err := os.WriteFile(path, []byte("this-content-is-longer-than-the-original"), 0o644); err != nil {
		t.Fatal(err)
	}
	newMtime := mtime.Add(time.Hour)
	if err := os.Chtimes(path, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	srv, _ := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedFileChanged != 1 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want SkippedFileChanged=1 Pruned=0", stats)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the changed file untouched: %v", err)
	}
}

func TestPassRefusesFileOutsideLocalEditRoot(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}

	// A fabricated row whose LocalPath escapes LocalEditRoot entirely --
	// normal ingest never produces this, but the containment check must
	// refuse it regardless of what the server says.
	outsideDir := filepath.Join(filepath.Dir(root), "outside")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "0190f1a2-0000-7000-8000-000000000006"
	path := filepath.Join(outsideDir, "escaped.mov")
	insertDoneRow(t, store, uuid, path, "should-never-be-touched", 48, queue.KindMedia)

	srv, _ := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedOutsideRoot != 1 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want SkippedOutsideRoot=1 Pruned=0", stats)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the outside-root file untouched: %v", err)
	}
}

func TestPassDryRunDoesNotDelete(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000007"
	path := filepath.Join(root, "dryrun.mov")
	insertDoneRow(t, store, uuid, path, "would-be-pruned", 48, queue.KindMedia)

	srv, _ := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), true /* dryRun */)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1 (dry-run still counts what it WOULD prune)", stats.Pruned)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("dry-run must never delete: %v", err)
	}
	if !strings.Contains(buf.String(), "DRY-RUN") {
		t.Errorf("expected a DRY-RUN marker in output: %q", buf.String())
	}
}

func TestPassSkipsSidecarNeverAskedServer(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}
	uuid := "0190f1a2-0000-7000-8000-000000000008"
	path := filepath.Join(root, "sidecar.xmp")
	insertDoneRow(t, store, uuid, path, "<xmp/>", 48, queue.KindSidecar)

	srv, asked := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{uuid: verifiedEntry(uuid)})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedSidecar != 1 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want SkippedSidecar=1 Pruned=0", stats)
	}
	if len(*asked) != 0 {
		t.Errorf("asked = %v, want a sidecar's NodeUUID never looked up (it never has a media_nodes row)", *asked)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the sidecar file untouched: %v", err)
	}
}

func TestPassNotFoundOrUnverifiedNeverPruned(t *testing.T) {
	cfg, store, root := pruneTestFixture(t)
	cfg.Prune = config.PruneConfig{Enabled: true, MinAgeHours: 24}

	notFoundUUID := "0190f1a2-0000-7000-8000-000000000009"
	notFoundPath := filepath.Join(root, "not-found.mov")
	insertDoneRow(t, store, notFoundUUID, notFoundPath, "x", 48, queue.KindMedia)

	unverifiedUUID := "0190f1a2-0000-7000-8000-00000000000a"
	unverifiedPath := filepath.Join(root, "unverified.mov")
	insertDoneRow(t, store, unverifiedUUID, unverifiedPath, "y", 48, queue.KindMedia)

	srv, _ := nodeStatusServer(t, map[string]branchdam.NodeStatusEntry{
		// notFoundUUID deliberately absent from the map -> server reports Found=false.
		unverifiedUUID: {NodeUUID: unverifiedUUID, Found: true, LifecycleState: "ACTIVE", Tier: "TIER0_LOCAL_STAGING", Verified: false},
	})
	_ = srv
	client := branchdam.New(srv.URL, "test-key")

	var buf bytes.Buffer
	stats, err := Pass(context.Background(), &buf, client, store, cfg, time.Now(), false)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.SkippedNotVerified != 2 || stats.Pruned != 0 {
		t.Errorf("stats = %+v, want SkippedNotVerified=2 Pruned=0", stats)
	}
	if _, err := os.Stat(notFoundPath); err != nil {
		t.Errorf("expected not-found file untouched: %v", err)
	}
	if _, err := os.Stat(unverifiedPath); err != nil {
		t.Errorf("expected unverified/wrong-tier file untouched: %v", err)
	}
}
