package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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
// #197+#198 wiring). The two callbacks (onSyncComplete updates the
// runner's in-memory carry-forward; onSaveMemberships persists to
// runtime.json) must both be non-nil and must do the right thing when
// invoked.
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
	if syncer.onSyncComplete != nil {
		t.Error("pre-wiring onSyncComplete is set; should only be set by wireResolveSyncer")
	}

	r := tray.NewRunner(&fakeIngester{}, nil, "")
	wireResolveSyncer(r, syncer)

	if syncer.onSyncComplete == nil {
		t.Fatal("after wireResolveSyncer: onSyncComplete is nil -- sync pass cannot update in-memory carry-forward")
	}
	if syncer.onSaveMemberships == nil {
		t.Fatal("after wireResolveSyncer: onSaveMemberships is nil -- next restart loses the delta baseline")
	}

	// onSyncComplete must update the runner's in-memory carry-forward.
	// This is the bridge that keeps hasPrev=true on subsequent passes.
	fresh := []tray.SyncMembershipEntry{
		{MediaPath: "/storage/clip1.mp4", TimelineID: "tl-1"},
		{MediaPath: "/storage/clip2.mp4", TimelineID: "tl-1"},
	}
	syncer.onSyncComplete(fresh)

	got := r.LastResolveMemberships()
	if len(got) != 2 {
		t.Fatalf("after onSyncComplete: LastResolveMemberships len = %d, want 2", len(got))
	}
	if got[0].MediaPath != "/storage/clip1.mp4" || got[1].TimelineID != "tl-1" {
		t.Errorf("after onSyncComplete: LastResolveMemberships = %+v", got)
	}

	// onSaveMemberships must persist the membership set to runtime.json.
	// Same contract as onSuccessfulHandshake: a failing save is logged,
	// never blocks the sync pass -- but the call must succeed for a
	// happy-path round-trip.
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
