package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	runtimeState "github.com/s3ntin3l8/branchdam-agent/internal/runtime"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// fakeIngester implements tray.Ingester minimally for the
// wireRuntimeStateWithOps tests. The drain-pass success/failure
// shape is what the wiring logic depends on, not the ingest
// shape, so this is a trivial stub.
type fakeIngester struct{}

func (f *fakeIngester) IngestCard(_ context.Context, _ string) (ingest.CardResult, error) {
	return ingest.CardResult{}, nil
}

func (f *fakeIngester) IngestCardOffline(_ context.Context, _ string) (ingest.OfflineCardResult, error) {
	return ingest.OfflineCardResult{}, nil
}

// fakeDrainer implements tray.Drainer with a controllable
// DrainSummary return. wireRuntimeStateWithOps tests use this
// to drive TriggerDrain through the save-callback site.
type fakeDrainer struct {
	summary tray.DrainSummary
	calls   atomic.Int32
}

func (f *fakeDrainer) Drain(_ context.Context) (tray.DrainSummary, error) {
	f.calls.Add(1)
	return f.summary, nil
}

// TestWireRuntimeStateWithOpsPathError pins branch 1 of
// wireRuntimeStateWithOps: a Path() error means the cross-restart
// signal is unavailable for this session. The function must warn
// and return without seeding lastDrain or wiring the save
// callback. A direct test of this branch is impossible without the
// runtimeStateOps indirection, because the production Path() can
// only return one specific kind of error per platform.
func TestWireRuntimeStateWithOpsPathError(t *testing.T) {
	r := tray.NewRunner(&fakeIngester{}, nil, "")

	wireRuntimeStateWithOps(r, runtimeStateOps{
		Path: func() (string, error) { return "", errors.New("no $HOME") },
		Load: func(string) (runtimeState.State, error) {
			t.Fatal("Load should not be called when Path errors")
			return runtimeState.State{}, nil
		},
		Save: func(string, runtimeState.State) error {
			t.Fatal("Save should not be called when Path errors")
			return nil
		},
	})

	st := r.Status(tray.UpdateStatus{})
	if !st.LastHandshakeAt.IsZero() {
		t.Errorf("after Path() error: LastHandshakeAt = %v, want zero (no seed should be attempted)", st.LastHandshakeAt)
	}
	if st.HasDrained {
		t.Error("after Path() error: HasDrained=true, want false (no seed should be attempted)")
	}
}

// TestWireRuntimeStateWithOpsLoadError pins branch 2: Load
// errored but the file *exists* (the runtime file was on disk
// but unreadable -- typically a permission revocation). The
// function must log at ERROR, skip seeding, and skip wiring
// the save callback. Skipping the save callback is the load-
// bearing behavior: a successful drain that wrote a fresh
// stamp would silently overwrite the unreadable file with
// the freshest one, hiding the underlying permission problem
// from the next session's Load.
func TestWireRuntimeStateWithOpsLoadError(t *testing.T) {
	r := tray.NewRunner(&fakeIngester{}, nil, "")

	wireRuntimeStateWithOps(r, runtimeStateOps{
		Path: func() (string, error) { return "/some/path/runtime.json", nil },
		Load: func(string) (runtimeState.State, error) { return runtimeState.State{}, errors.New("permission denied") },
		Save: func(string, runtimeState.State) error {
			t.Fatal("Save should NOT be wired when Load errored (skip prevents overwriting the unreadable file)")
			return nil
		},
	})

	st := r.Status(tray.UpdateStatus{})
	if !st.LastHandshakeAt.IsZero() {
		t.Errorf("after Load() error: LastHandshakeAt = %v, want zero (no seed attempted when the prior state is unreadable)", st.LastHandshakeAt)
	}
	if st.HasDrained {
		t.Error("after Load() error: HasDrained=true, want false (no seed attempted when the prior state is unreadable)")
	}
}

// TestWireRuntimeStateWithOpsSeedFromDisk pins branch 3: the
// happy path. A prior session's last successful handshake is
// on disk, the file is readable, and the current session must
// surface it through Status().LastHandshakeAt + seed the
// in-memory carry-forward. The save callback must also be
// wired so this session's first successful drain writes
// the new stamp back.
func TestWireRuntimeStateWithOpsSeedFromDisk(t *testing.T) {
	prior := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := tray.NewRunner(&fakeIngester{}, nil, "")

	var savedAt time.Time
	var saveCalls atomic.Int32
	wireRuntimeStateWithOps(r, runtimeStateOps{
		Path: func() (string, error) { return "/some/path/runtime.json", nil },
		Load: func(string) (runtimeState.State, error) { return runtimeState.State{LastHandshakeAt: prior}, nil },
		Save: func(_ string, st runtimeState.State) error {
			saveCalls.Add(1)
			savedAt = st.LastHandshakeAt
			return nil
		},
	})

	st := r.Status(tray.UpdateStatus{})
	if !st.LastHandshakeAt.Equal(prior) {
		t.Errorf("after Load returns a stamp: LastHandshakeAt = %v, want %v", st.LastHandshakeAt, prior)
	}
	if !st.HasDrained {
		t.Error("after Load returns a stamp: HasDrained=false, want true (a persisted stamp IS evidence we have ever drained)")
	}

	// The save callback must be wired: a subsequent successful
	// drain should invoke it.
	fresh := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	fd := &fakeDrainer{summary: tray.DrainSummary{At: fresh, HandshakeOK: true, LastHandshakeAt: fresh}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(t.Context())

	if saveCalls.Load() != 1 {
		t.Errorf("Save calls = %d, want 1 (the save callback must be wired when a prior stamp was loaded)", saveCalls.Load())
	}
	if !savedAt.Equal(fresh) {
		t.Errorf("Save t = %v, want %v (the save callback should be invoked with the just-stamped time, not the persisted one)", savedAt, fresh)
	}
}

// TestWireRuntimeStateWithOpsFreshInstall pins branch 4: the
// "never" sentinel. No prior successful handshake on disk --
// fresh install, or the prior runtime file was missing/empty/
// corrupt (Load returns zero, nil for those). The save callback
// must still be wired so this session establishes the first
// cross-restart signal.
func TestWireRuntimeStateWithOpsFreshInstall(t *testing.T) {
	r := tray.NewRunner(&fakeIngester{}, nil, "")

	var savedAt time.Time
	var saveCalls atomic.Int32
	wireRuntimeStateWithOps(r, runtimeStateOps{
		Path: func() (string, error) { return "/some/path/runtime.json", nil },
		Load: func(string) (runtimeState.State, error) { return runtimeState.State{}, nil },
		Save: func(_ string, st runtimeState.State) error {
			saveCalls.Add(1)
			savedAt = st.LastHandshakeAt
			return nil
		},
	})

	st := r.Status(tray.UpdateStatus{})
	if !st.LastHandshakeAt.IsZero() {
		t.Errorf("after Load returns zero: LastHandshakeAt = %v, want zero (no seed for the 'never' sentinel)", st.LastHandshakeAt)
	}
	if st.HasDrained {
		t.Error("after Load returns zero: HasDrained=true, want false (no seed for the 'never' sentinel)")
	}

	// Save callback must be wired so the first successful drain
	// establishes the cross-restart signal for the next session.
	fresh := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	fd := &fakeDrainer{summary: tray.DrainSummary{At: fresh, HandshakeOK: true, LastHandshakeAt: fresh}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(t.Context())

	if saveCalls.Load() != 1 {
		t.Errorf("Save calls = %d, want 1 (the save callback must be wired even on a fresh install so the first successful drain establishes the cross-restart signal)", saveCalls.Load())
	}
	if !savedAt.Equal(fresh) {
		t.Errorf("Save t = %v, want %v", savedAt, fresh)
	}
}

// TestWireRuntimeStateWithOpsSaveErrorPropagatesThroughDrain
// pins the "failing WriteFile must not block the drain" contract
// from issue #149, end-to-end through the cmd wiring. The
// save callback is allowed to return an error; the drain pass
// itself must still complete with HandshakeOK=true. This is
// the integration of the TriggerDrain-side guard (recover
// panic, log error) with the wiring's on-success registration.
func TestWireRuntimeStateWithOpsSaveErrorPropagatesThroughDrain(t *testing.T) {
	r := tray.NewRunner(&fakeIngester{}, nil, "")

	wireRuntimeStateWithOps(r, runtimeStateOps{
		Path: func() (string, error) { return "/some/path/runtime.json", nil },
		Load: func(string) (runtimeState.State, error) {
			return runtimeState.State{LastHandshakeAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}, nil
		},
		Save: func(string, runtimeState.State) error { return errors.New("disk full") },
	})

	fresh := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	fd := &fakeDrainer{summary: tray.DrainSummary{At: fresh, HandshakeOK: true, LastHandshakeAt: fresh}}
	r.SetQueueDeps(nil, fd, nil)

	summary, ran := r.TriggerDrain(t.Context())
	if !ran {
		t.Fatal("expected TriggerDrain ran=true even when the save callback returns an error")
	}
	if !summary.HandshakeOK {
		t.Error("expected HandshakeOK=true even when the save callback returns an error (the drain pass must not be corrupted by a save failure)")
	}
	if !summary.LastHandshakeAt.Equal(fresh) {
		t.Errorf("summary.LastHandshakeAt = %v, want %v (a save failure must not erase the just-stamped successful handshake)", summary.LastHandshakeAt, fresh)
	}
}

// TestWireRuntimeStatePersistenceRealWiringReachesTheDisk is
// the integration test for the production wiring: invoke
// wireRuntimeStatePersistence (not the *_WithOps variant)
// with a real Runner and a real (test-temp) runtime file, and
// confirm the runner's Status reflects the seeded stamp. This
// pins the indirection -- if a future refactor renames or
// drops the ops struct, this test breaks.
func TestWireRuntimeStatePersistenceRealWiringReachesTheDisk(t *testing.T) {
	prior := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	// Run with a tempdir-backed HOME/XDG_STATE_HOME so the real
	// runtimeState.Path() resolves to a path we control. The
	// actual Save will write into that tempdir.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	rtPath, err := runtimeState.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeState.Save(rtPath, runtimeState.State{LastHandshakeAt: prior}); err != nil {
		t.Fatal(err)
	}

	r := tray.NewRunner(&fakeIngester{}, nil, "")
	wireRuntimeStatePersistence(r)

	st := r.Status(tray.UpdateStatus{})
	if !st.LastHandshakeAt.Equal(prior) {
		t.Errorf("after wireRuntimeStatePersistence with a real stamped file: LastHandshakeAt = %v, want %v", st.LastHandshakeAt, prior)
	}
}

// TestWireResolveSyncerWiresDeltaDetectionCallbacks verifies the resolve
// integration's delta-detection callbacks are populated after wiring.
// Without this, every sync pass would re-emit every edge (hasPrev=false)
// and never persist -- the exact regression that motivated issue #184's
// full hand-off chain (#195 runtime state, #196 delta detection,
// #197+#198 wiring). onSaveMemberships persists to runtime.json and
// must be non-nil and do the right thing when invoked.
func TestWireResolveSyncerWiresDeltaDetectionCallbacks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	cfg := config.Config{
		Server:  config.ServerConfig{BaseURL: "http://localhost:8080", APIKey: "0123456789abcdef0123456789abcdef"},
		AgentID: "test-agent",
		Integrations: config.IntegrationsConfig{
			NodeIndexPath: "/dev/null/non-existent-but-Ready-only-checks-string",
			ResolveDB: config.ResolveDBConfig{
				Enabled:     true,
				DatabaseURL: "file:/dev/null/db?mode=ro",
				DryRun:      true, // skip the node index existence check
			},
		},
	}

	deps, syncer := buildIntegrationDeps(cfg, nil)
	if syncer == nil {
		t.Fatal("buildIntegrationDeps returned nil syncer for an enabled ResolveDB config")
	}
	if _, ok := deps[tray.IntegrationResolveDB]; !ok {
		t.Fatal("ResolveDB not in built deps map")
	}

	// Pre-wiring: the freshly-built syncer has no callbacks set.
	// (We can't read these directly without exposing them, but
	// we know wireResolveSyncer is what sets them.)
	if syncer.prevMemberships != nil {
		t.Errorf("pre-wiring prevMemberships = %v, want nil", syncer.prevMemberships)
	}
	if syncer.onSaveMemberships != nil {
		t.Error("pre-wiring onSaveMemberships is set; should only be set by wireResolveSyncer")
	}

	r := tray.NewRunner(&fakeIngester{}, nil, "")
	wireResolveSyncer(r, syncer)

	if syncer.onSaveMemberships == nil {
		t.Fatal("after wireResolveSyncer: onSaveMemberships is nil -- next restart loses the delta baseline")
	}

	// onSaveMemberships must persist the membership set to runtime.json.
	// Same contract as onSuccessfulHandshake: a failing save is logged,
	// never blocks the sync pass -- but the call must succeed for a
	// happy-path round-trip.
	fresh := []tray.SyncMembershipEntry{
		{MediaPath: "/storage/clip1.mp4", TimelineID: "tl-1"},
		{MediaPath: "/storage/clip2.mp4", TimelineID: "tl-1"},
	}
	if err := syncer.onSaveMemberships(fresh); err != nil {
		t.Fatalf("onSaveMemberships failed on happy path: %v", err)
	}
	rtPath, err := runtimeState.Path()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtimeState.Load(rtPath)
	if err != nil {
		t.Fatalf("runtimeState.Load after onSaveMemberships: %v", err)
	}
	if len(rt.ResolveEmittedMemberships) != 2 {
		t.Errorf("runtime.json ResolveEmittedMemberships len = %d, want 2 (next-restart seed missing)", len(rt.ResolveEmittedMemberships))
	}
	if rt.ResolveEmittedMemberships[0].MediaPath != "/storage/clip1.mp4" {
		t.Errorf("runtime.json [0] = %+v, want {clip1.mp4 tl-1}", rt.ResolveEmittedMemberships[0])
	}
}

// TestBuildIntegrationDepsReturnsNilResolveSyncerWhenNotReady confirms
// the second return value from buildIntegrationDeps is the honest
// "not configured" signal when the resolve integration isn't Ready.
// Without this, settings.reload() and runTrayCmd would crash calling
// wireResolveSyncer(runner, nil).
func TestBuildIntegrationDepsReturnsNilResolveSyncerWhenNotReady(t *testing.T) {
	cfg := config.Config{
		Integrations: config.IntegrationsConfig{
			// No ResolveDB config at all -- Ready must return false.
			Luminar: config.CatalogSyncConfig{Enabled: true, CatalogPath: "/c.db", DryRun: true},
		},
	}
	_, syncer := buildIntegrationDeps(cfg, nil)
	if syncer != nil {
		t.Errorf("buildIntegrationDeps returned non-nil resolve syncer for an integration that's not Ready: %T", syncer)
	}
}

// resolveSchemaDdl mirrors the schema from internal/resolve/resolve_test.go
// so cmd tests can stand up a real Resolve SQLite DB without reaching
// into the internal package's test helpers (which are package-private).
// Schema column ordering and types must match exactly -- the syncer's
// query.go reads them in this shape.
const resolveSchemaDdl = `
CREATE TABLE IF NOT EXISTS "Sm2Timeline" (
	"Sm2Timeline_id" TEXT PRIMARY KEY,
	"Name" TEXT
);
CREATE TABLE IF NOT EXISTS "Sm2Sequence" (
	"Sm2Sequence_id" TEXT PRIMARY KEY,
	"Sm2Timeline_id" TEXT
);
CREATE TABLE IF NOT EXISTS "Sm2TiTrack" (
	"Sm2TiTrack_id" TEXT PRIMARY KEY,
	"Type" INTEGER,
	"Sequence" TEXT
);
CREATE TABLE IF NOT EXISTS "Sm2TiItem" (
	"Sm2TiItem_id" TEXT PRIMARY KEY,
	"Name" TEXT,
	"MediaFilePath" TEXT,
	"In" TEXT,
	"Start" TEXT,
	"Duration" TEXT,
	"Sm2TiTrack_id" TEXT
);
`

// buildTestResolveDB creates a tempdir-backed SQLite DB with the Resolve
// schema and one clip, returning its path. Pass multiple clips via
// additionalClip to extend the DB after the first sync.
func buildTestResolveDB(t *testing.T, timeline, seq, track, item, mediaPath string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "resolve.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(context.Background(), resolveSchemaDdl); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`INSERT INTO "Sm2Timeline" VALUES (?, ?)`,
		timeline, "Master"); err != nil {
		t.Fatalf("insert timeline: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`INSERT INTO "Sm2Sequence" VALUES (?, ?)`,
		seq, timeline); err != nil {
		t.Fatalf("insert sequence: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`INSERT INTO "Sm2TiTrack" VALUES (?, 0, ?)`,
		track, seq); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if _, err := raw.ExecContext(context.Background(),
		`INSERT INTO "Sm2TiItem" VALUES (?, ?, ?, ?, ?, ?, ?)`,
		item, "clip1.mp4", mediaPath, "0", "0", "100", track); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	return dbPath
}

// addClipToResolveDB inserts an additional clip into an existing test
// Resolve DB. Used to simulate a clip being added mid-session: pass 1
// sees N clips, pass 2 (after addClipToResolveDB) sees N+1.
func addClipToResolveDB(t *testing.T, dbPath, itemID, mediaPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(context.Background(),
		`INSERT INTO "Sm2TiItem" VALUES (?, ?, ?, ?, ?, ?, ?)`,
		itemID, "clip2.mp4", mediaPath, "0", "100", "50", "trk-1"); err != nil {
		t.Fatalf("insert item: %v", err)
	}
}

// TestResolveDBSyncerAdvancesPrevMembershipsAcrossPasses is the
// in-session regression guard flagged in the review: the OnSaveMemberships
// bridge must advance s.prevMemberships after each successful pass,
// otherwise every pass compares against the same startup snapshot --
// a clip added mid-session is re-emitted as a duplicate edge on each
// subsequent pass, and a removed clip is re-logged as removed every
// remaining pass.
//
// The test wires a real resolveDBSyncer against a real SQLite Resolve
// DB and a real (test-temp) node index JSON, then runs Sync() twice --
// pass 1 sees one clip, pass 2 sees two (added via addClipToResolveDB
// plus a node-index file rewrite). After pass 1, syncer.prevMemberships
// must contain clip1 and the runner's carry-forward must match. After
// pass 2, both must contain BOTH clips -- not just the newly-emitted
// clip2 -- so clip1 is correctly classified as Unchanged and skip-emit
// on pass 2 (the load-bearing in-session delta detection behavior).
//
// DryRun=false is required so the OnSaveMemberships bridge actually
// fires (the !s.DryRun guard in resolve.Syncer prevents it from running
// during dry runs -- otherwise a dry-run would pollute the in-session
// baseline with simulated emissions that the real pass would skip as
// already-emitted). We inject a no-op fake client so PostEdgeAttached
// returns success without a real server.
func TestResolveDBSyncerAdvancesPrevMembershipsAcrossPasses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())

	dbPath := buildTestResolveDB(t,
		"tl-1", "seq-1", "trk-1", "item-1",
		`D:\Videos\clip1.mp4`)

	// Build the node-index file in a tempdir we control so we can
	// rewrite it between passes (the resolve syncer loads it once at
	// the top of Sync(), so we don't need to mutate it in-memory).
	indexDir := t.TempDir()
	indexPath := filepath.Join(indexDir, "node-index.json")
	indexEntries := map[string]string{
		"/storage/videos/clip1.mp4": "node-1",
	}
	if err := os.WriteFile(indexPath, indexJSON(indexEntries), 0o644); err != nil {
		t.Fatalf("write initial node index: %v", err)
	}

	cfg := config.Config{
		AgentID: "test-agent",
		Integrations: config.IntegrationsConfig{
			NodeIndexPath: indexPath,
			ResolveDB: config.ResolveDBConfig{
				Enabled:     true,
				DatabaseURL: "file:" + dbPath + "?mode=ro",
				DryRun:      false, // required so the OnSaveMemberships bridge fires
				PathRewrites: []config.ResolvePathRewrite{
					{From: `D:\Videos\`, To: "/storage/videos/"},
				},
			},
		},
	}

	_, syncer := buildIntegrationDeps(cfg, nil)
	if syncer == nil {
		t.Fatal("buildIntegrationDeps returned nil syncer for an enabled ResolveDB config")
	}
	// Inject a fake client so PostEdgeAttached returns success without
	// a real branchDAM server. Required because DryRun=false needs a
	// working client to exercise the OnSaveMemberships bridge.
	syncer.client = fakeResolveClient{}
	syncer.virtualEmitter = fakeResolveClient{}

	r := tray.NewRunner(&fakeIngester{}, nil, "")
	wireResolveSyncer(r, syncer)

	// Pre-pass: no baseline (startup fresh).
	if len(syncer.prevMemberships) != 0 {
		t.Errorf("pre-pass syncer.prevMemberships len = %d, want 0", len(syncer.prevMemberships))
	}

	// Pass 1: emits clip1.
	summary1, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if summary1.Emitted != 1 {
		t.Errorf("pass 1 Emitted = %d, want 1", summary1.Emitted)
	}
	if summary1.Errors != 0 {
		t.Errorf("pass 1 Errors = %d, want 0", summary1.Errors)
	}
	if got := syncer.prevMemberships; len(got) != 1 {
		t.Errorf("after pass 1, syncer.prevMemberships len = %d, want 1 -- the in-session baseline did not advance", len(got))
	} else if got[0].MediaPath != `D:\Videos\clip1.mp4` {
		t.Errorf("after pass 1, syncer.prevMemberships[0] = %+v, want {clip1.mp4 tl-1}", got[0])
	}

	// Mid-session: add a new clip to the Resolve DB and the node index.
	// Without the in-session fix, pass 2 would either re-emit clip1
	// (if the baseline was reset to nil) or fail to see clip2 as new
	// (if the OnSaveMemberships bridge never updated s.prevMemberships).
	addClipToResolveDB(t, dbPath, "item-2", `D:\Videos\clip2.mp4`)
	indexEntries["/storage/videos/clip2.mp4"] = "node-2"
	if err := os.WriteFile(indexPath, indexJSON(indexEntries), 0o644); err != nil {
		t.Fatalf("rewrite node index: %v", err)
	}

	// Pass 2: clip1 unchanged (no re-emit), clip2 new.
	summary2, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if summary2.Emitted != 1 {
		// With the fix, only clip2 newly emits (clip1 is unchanged,
		// so it's skipped). Without the fix, pass 2 would either emit
		// nothing (if the baseline was over-broad and excluded both
		// clips) or re-emit clip1 (if the baseline was reset to nil).
		t.Errorf("pass 2 Emitted = %d, want 1 (only clip2 should emit; clip1 is unchanged)", summary2.Emitted)
	}
	if summary2.Errors != 0 {
		t.Errorf("pass 2 Errors = %d, want 0", summary2.Errors)
	}

	// The in-session baseline should now hold BOTH clips, not just the
	// newly-emitted clip2. This is the load-bearing check: the
	// OnSaveMemberships bridge must include unchanged clips in the
	// next-pass baseline, otherwise an all-unchanged pass would emit
	// nothing and the baseline would reset to empty on the next sync.
	got := syncer.prevMemberships
	if len(got) != 2 {
		t.Fatalf("after pass 2, syncer.prevMemberships len = %d, want 2 (clip1 + clip2): %+v", len(got), got)
	}
	gotPaths := map[string]string{}
	for _, m := range got {
		gotPaths[m.MediaPath] = m.TimelineID
	}
	wantPaths := map[string]string{
		`D:\Videos\clip1.mp4`: "tl-1",
		`D:\Videos\clip2.mp4`: "tl-1",
	}
	for path, timeline := range wantPaths {
		if gotPaths[path] != timeline {
			t.Errorf("after pass 2, missing or wrong entry for %q (got %+v, want timeline=%q)", path, gotPaths[path], timeline)
		}
	}
}

// fakeResolveClient satisfies both resolve.EdgeAttacher and
// resolve.VirtualNodeEmitter with no-op success responses. Used by
// the in-session baseline regression test to exercise the
// OnSaveMemberships bridge without a real branchDAM server.
type fakeResolveClient struct{}

func (fakeResolveClient) PostEdgeAttached(_ context.Context, _ string, _ branchdam.EdgeAttachedPayload) (*branchdam.EventResponse, error) {
	return &branchdam.EventResponse{EventID: "fake"}, nil
}

func (fakeResolveClient) PostVirtualNodeCreated(_ context.Context, _ string, _ branchdam.VirtualNodeCreated) (*branchdam.EventResponse, error) {
	return &branchdam.EventResponse{EventID: "fake"}, nil
}

// indexJSON serializes a map[string]string to a JSON object body,
// matching nodeindex.FileIndex's on-disk shape.
func indexJSON(entries map[string]string) []byte {
	var buf []byte
	buf = append(buf, '{')
	first := true
	for k, v := range entries {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = append(buf, k...)
		buf = append(buf, `":"`...)
		buf = append(buf, v...)
		buf = append(buf, '"')
	}
	buf = append(buf, '}')
	return buf
}
