package tray

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// fakeIngester substitutes internal/ingest.Engine for tests, matching the
// pattern internal/ingest.Engine's own nodeCreator interface and
// cmd/branchdam-agent/preflight.go's helloCaller use.
type fakeIngester struct {
	result        ingest.CardResult
	offlineResult ingest.OfflineCardResult
	err           error
	offlineErr    error
	calls         []string
	offlineCalls  []string
}

func (f *fakeIngester) IngestCard(_ context.Context, cardRoot string) (ingest.CardResult, error) {
	f.calls = append(f.calls, cardRoot)
	return f.result, f.err
}

func (f *fakeIngester) IngestCardOffline(_ context.Context, cardRoot string) (ingest.OfflineCardResult, error) {
	f.offlineCalls = append(f.offlineCalls, cardRoot)
	return f.offlineResult, f.offlineErr
}

func TestTriggerIngestCountsOutcomes(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "a.jpg"},
		{SourcePath: "b.xmp", Skipped: true},
		{SourcePath: "c.jpg", Err: errors.New("boom")},
	}}}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if summary.Submitted != 1 || summary.Skipped != 1 || summary.Failed != 1 {
		t.Fatalf("got %+v, want 1/1/1", summary)
	}
	if summary.OK() {
		t.Error("expected OK()=false when a file failed")
	}
	if len(fi.calls) != 1 || fi.calls[0] != "/media/card" {
		t.Errorf("expected IngestCard called once with the card path, got %v", fi.calls)
	}

	st := r.Status(UpdateStatus{})
	if st.LastIngest == nil || st.LastIngest.Failed != 1 {
		t.Fatalf("expected Status() to reflect the last TriggerIngest call, got %+v", st.LastIngest)
	}
}

func TestTriggerIngestEngineError(t *testing.T) {
	fi := &fakeIngester{err: errors.New("walk failed")}
	r := NewRunner(fi, nil, "")

	summary := r.TriggerIngest(context.Background(), "/media/card")
	if summary.Err == nil {
		t.Fatal("expected an error to be recorded")
	}
	if summary.OK() {
		t.Error("expected OK()=false on engine error")
	}
}

func TestTriggerIngestOfflineFallbackWhenArchiveUnreachable(t *testing.T) {
	fi := &fakeIngester{
		offlineResult: ingest.OfflineCardResult{
			Files: []ingest.OfflineFileResult{
				{SourcePath: "a.jpg", LocalPath: "/scratch/a.jpg"},
				{SourcePath: "b.xmp", Skipped: true},
			},
		},
	}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")
	r.SetArchiveRoot("/nas/archive")
	r.SetArchiveProber(func(ctx context.Context, archiveRoot string) bool {
		return false // simulate NAS unreachable
	})
	qr := &fakeQueueReader{counts: QueueCounts{AwaitingUpload: 2}}
	r.SetQueueDeps(qr, nil, nil)

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if !summary.Offline {
		t.Error("expected summary.Offline=true when falling back to offline ingest")
	}
	if summary.Submitted != 1 || summary.Skipped != 1 || summary.Failed != 0 {
		t.Fatalf("got summary %+v, want 1 submitted, 1 skipped, 0 failed", summary)
	}
	if !summary.OK() {
		t.Error("expected summary.OK()=true when offline files queued cleanly")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard NOT called, got %v", fi.calls)
	}
	if len(fi.offlineCalls) != 1 || fi.offlineCalls[0] != "/media/card" {
		t.Errorf("expected IngestCardOffline called once with /media/card, got %v", fi.offlineCalls)
	}

	st := r.Status(UpdateStatus{})
	if !st.LastIngestWasOffline {
		t.Error("expected st.LastIngestWasOffline=true")
	}
	if st.PendingOfflineCount != 2 {
		t.Errorf("expected st.PendingOfflineCount=2, got %d", st.PendingOfflineCount)
	}
}

func TestTriggerIngestOfflineFallbackUnconfiguredShowsError(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")
	r.SetArchiveRoot("/nas/archive")
	r.SetArchiveProber(func(ctx context.Context, archiveRoot string) bool {
		return false // simulate NAS unreachable
	})
	// Queue is NOT configured (SetQueueDeps not called)

	var notifiedTitle, notifiedMsg string
	r.SetErrorNotifier(func(title, message string) {
		notifiedTitle = title
		notifiedMsg = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if summary.Err == nil {
		t.Fatal("expected summary.Err when NAS unreachable and queue not configured")
	}
	wantMsg := "NAS unreachable. Set offline.queueDbPath to enable field ingest"
	if summary.Err.Error() != wantMsg {
		t.Errorf("got err %q, want %q", summary.Err.Error(), wantMsg)
	}
	if len(fi.calls) != 0 || len(fi.offlineCalls) != 0 {
		t.Errorf("expected neither IngestCard nor IngestCardOffline to be called")
	}
	if notifiedMsg != wantMsg {
		t.Errorf("expected notifyError called with message %q, got %q (title %q)", wantMsg, notifiedMsg, notifiedTitle)
	}
}

func TestTriggerIngestProberTrueRunsOnline(t *testing.T) {
	fi := &fakeIngester{
		result: ingest.CardResult{
			Files: []ingest.FileResult{
				{SourcePath: "a.jpg"},
			},
		},
	}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")
	r.SetArchiveRoot("/nas/archive")
	r.SetArchiveProber(func(ctx context.Context, archiveRoot string) bool {
		return true // simulate NAS reachable
	})
	qr := &fakeQueueReader{counts: QueueCounts{AwaitingUpload: 1}}
	r.SetQueueDeps(qr, nil, nil)

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if summary.Offline {
		t.Error("expected summary.Offline=false when prober returns true")
	}
	if len(fi.calls) != 1 || fi.calls[0] != "/media/card" {
		t.Errorf("expected IngestCard called once, got %v", fi.calls)
	}
	if len(fi.offlineCalls) != 0 {
		t.Errorf("expected IngestCardOffline NOT called, got %v", fi.offlineCalls)
	}

	st := r.Status(UpdateStatus{})
	if st.LastIngestWasOffline {
		t.Error("expected st.LastIngestWasOffline=false")
	}
}

func TestStatusQueueStatusNotConfiguredByDefault(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{Enabled: true, Checked: true, CurrentVersion: "1.0.0"})
	if st.QueueStatus.Configured {
		t.Error("expected QueueStatus.Configured=false when SetQueueDeps was never called -- never a fabricated number")
	}
	if st.QueueStatus.Counts != (QueueCounts{}) {
		t.Errorf("expected a zero-value Counts when not configured, got %+v", st.QueueStatus.Counts)
	}
	if st.SelfUpdate.Note() != "up to date (1.0.0)" {
		t.Errorf("got %q", st.SelfUpdate.Note())
	}
}

// fakeQueueReader/fakeDrainer/fakePruner substitute the real internal/queue
// and internal/prune wiring for Runner tests -- same pattern as
// fakeIngester.
type fakeQueueReader struct {
	counts QueueCounts
	err    error
}

func (f *fakeQueueReader) Counts(_ context.Context) (QueueCounts, error) {
	return f.counts, f.err
}

type fakeDrainer struct {
	summary DrainSummary
	err     error
	started chan struct{}
	release chan struct{}
	calls   int
}

func (f *fakeDrainer) Drain(_ context.Context) (DrainSummary, error) {
	f.calls++
	if f.started != nil {
		close(f.started)
		<-f.release
	}
	return f.summary, f.err
}

type fakePruner struct {
	summary PruneSummary
	err     error
	calls   int
}

func (f *fakePruner) Prune(_ context.Context) (PruneSummary, error) {
	f.calls++
	return f.summary, f.err
}

func TestStatusReflectsQueueCounts(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetQueueDeps(&fakeQueueReader{counts: QueueCounts{AwaitingUpload: 3, Failed: 1}}, nil, nil)

	st := r.Status(UpdateStatus{})
	if !st.QueueStatus.Configured {
		t.Fatal("expected Configured=true once SetQueueDeps is called with a non-nil reader")
	}
	if st.QueueStatus.Counts.Pending() != 3 || st.QueueStatus.Counts.Failed != 1 {
		t.Errorf("got %+v", st.QueueStatus.Counts)
	}
}

// fakeQueueReaderCapturesDeadline is a regression fixture for a Hermes
// review finding: Status() used to call Counts(context.Background()), an
// unbounded read on the 5s menu-refresh/status-page hot path that could
// hang the whole tray on a wedged or NAS-backed queue.db. It doesn't
// actually block (that would make this test slow); it just records
// whether the context it was handed carries a deadline.
type fakeQueueReaderCapturesDeadline struct {
	gotDeadline bool
}

func (f *fakeQueueReaderCapturesDeadline) Counts(ctx context.Context) (QueueCounts, error) {
	_, f.gotDeadline = ctx.Deadline()
	return QueueCounts{}, nil
}

func TestStatusBoundsQueueCountsRead(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fq := &fakeQueueReaderCapturesDeadline{}
	r.SetQueueDeps(fq, nil, nil)

	r.Status(UpdateStatus{})

	if !fq.gotDeadline {
		t.Error("expected Status() to pass a context with a deadline to QueueReader.Counts, not context.Background()")
	}
}

func TestStatusSurfacesQueueReadError(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	wantErr := errors.New("queue.db: disk I/O error")
	r.SetQueueDeps(&fakeQueueReader{err: wantErr}, nil, nil)

	st := r.Status(UpdateStatus{})
	if !st.QueueStatus.Configured {
		t.Fatal("expected Configured=true -- a read error is not the same as unconfigured")
	}
	if !errors.Is(st.QueueStatus.Err, wantErr) {
		t.Errorf("got Err=%v, want %v -- an unreadable queue.db must surface as an error, never a fabricated 0", st.QueueStatus.Err, wantErr)
	}
	if st.QueueStatus.Counts != (QueueCounts{}) {
		t.Error("expected zero Counts alongside a read error, not a stale or fabricated value")
	}
}

func TestTriggerDrainRunsAndRecordsLastDrain(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 2, RebasesDone: 1}}
	r.SetQueueDeps(nil, fd, nil)

	summary, ran := r.TriggerDrain(context.Background())
	if !ran {
		t.Fatal("expected TriggerDrain to run when a Drainer is configured and idle")
	}
	if summary.NodeCreatedSent != 2 || summary.RebasesDone != 1 {
		t.Errorf("got %+v", summary)
	}
	if summary.At.IsZero() {
		t.Error("expected TriggerDrain to stamp At, not leave it to the Drainer")
	}

	st := r.Status(UpdateStatus{})
	if st.QueueStatus.LastDrain == nil || st.QueueStatus.LastDrain.NodeCreatedSent != 2 {
		t.Errorf("expected Status to reflect the last drain, got %+v", st.QueueStatus.LastDrain)
	}
}

func TestTriggerDrainSkipsWhenNotConfigured(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, ran := r.TriggerDrain(context.Background())
	if ran {
		t.Error("expected TriggerDrain to report ran=false when no Drainer is wired")
	}
}

func TestTriggerDrainSkipsConcurrentPass(t *testing.T) {
	fd := &fakeDrainer{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetQueueDeps(nil, fd, nil)

	done := make(chan struct{})
	go func() {
		r.TriggerDrain(context.Background())
		close(done)
	}()
	<-fd.started

	if _, ran := r.TriggerDrain(context.Background()); ran {
		t.Error("expected a second TriggerDrain to skip (ran=false) while a pass is already running, not queue behind it")
	}

	close(fd.release)
	<-done
	if fd.calls != 1 {
		t.Errorf("expected exactly 1 Drain call, got %d", fd.calls)
	}
}

func TestTriggerDrainNeverBlocksOnIngestGate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1}}
	r.SetQueueDeps(nil, fd, nil)

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	done := make(chan struct{})
	go func() {
		_, ran := r.TriggerDrain(context.Background())
		if !ran {
			t.Error("expected TriggerDrain to run even while an ingest holds Runner.gate -- drainMu is a separate lock")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerDrain appears to be blocked on Runner.gate -- it must use its own drainMu")
	}

	close(release)
}

func TestTriggerPruneRunsAndRecordsLastPrune(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fp := &fakePruner{summary: PruneSummary{Pruned: 4, FreedBytes: 1024}}
	r.SetQueueDeps(nil, nil, fp)

	summary, ran := r.TriggerPrune(context.Background())
	if !ran {
		t.Fatal("expected TriggerPrune to run when a Pruner is configured and idle")
	}
	if summary.Pruned != 4 || summary.FreedBytes != 1024 {
		t.Errorf("got %+v", summary)
	}

	st := r.Status(UpdateStatus{})
	if !st.QueueStatus.PruneEnabled {
		t.Error("expected PruneEnabled=true once a Pruner is wired")
	}
	if st.QueueStatus.LastPrune == nil || st.QueueStatus.LastPrune.Pruned != 4 {
		t.Errorf("expected Status to reflect the last prune, got %+v", st.QueueStatus.LastPrune)
	}
}

func TestTriggerPruneSkipsWhenNotConfigured(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, ran := r.TriggerPrune(context.Background())
	if ran {
		t.Error("expected TriggerPrune to report ran=false when no Pruner is wired")
	}
	if r.Status(UpdateStatus{}).QueueStatus.PruneEnabled {
		t.Error("expected PruneEnabled=false when no Pruner is wired")
	}
}

func TestTriggerPruneSharesGateWithIngest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")
	fp := &fakePruner{summary: PruneSummary{Pruned: 1}}
	r.SetQueueDeps(nil, nil, fp)

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	if _, ran := r.TriggerPrune(context.Background()); ran {
		t.Error("expected TriggerPrune to skip (ran=false) while an ingest holds Runner.gate -- prune deletes from LocalEditRoot while ingest writes into it")
	}

	close(release)
}

// fakeIntegrationSyncer substitutes a real internal/luminar-backed syncer
// for Runner tests -- same pattern as fakeDrainer/fakePruner. started/
// release, when non-nil, block Sync until the test closes release, letting
// a test observe an in-flight pass.
type fakeIntegrationSyncer struct {
	summary SyncSummary
	err     error
	started chan struct{}
	release chan struct{}
	calls   int
}

func (f *fakeIntegrationSyncer) Sync(_ context.Context) (SyncSummary, error) {
	f.calls++
	if f.started != nil {
		close(f.started)
		<-f.release
	}
	return f.summary, f.err
}

func TestTriggerSyncRunsAndRecordsLastSync(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fs := &fakeIntegrationSyncer{summary: SyncSummary{PairsFound: 2, Emitted: 2}}
	r.SetIntegrationSyncers(map[IntegrationID]IntegrationSyncer{IntegrationLuminar: fs})

	summary, ran := r.TriggerSync(context.Background(), IntegrationLuminar)
	if !ran {
		t.Fatal("expected TriggerSync to run when a syncer is configured and idle")
	}
	if summary.PairsFound != 2 || summary.Emitted != 2 {
		t.Errorf("got %+v", summary)
	}
	if summary.At.IsZero() {
		t.Error("expected TriggerSync to stamp At, not leave it to the syncer")
	}

	st := r.Status(UpdateStatus{})
	var got *IntegrationStatus
	for i := range st.Integrations {
		if st.Integrations[i].ID == IntegrationLuminar {
			got = &st.Integrations[i]
		}
	}
	if got == nil {
		t.Fatal("expected an IntegrationStatus entry for IntegrationLuminar")
	}
	if !got.Registered {
		t.Error("expected Registered=true once a syncer is wired")
	}
	if got.LastSync == nil || got.LastSync.Emitted != 2 {
		t.Errorf("expected Status to reflect the last sync, got %+v", got.LastSync)
	}
}

func TestTriggerSyncSkipsWhenNotRegistered(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, ran := r.TriggerSync(context.Background(), IntegrationLuminar)
	if ran {
		t.Error("expected TriggerSync to report ran=false when no syncer is wired for this ID")
	}
	st := r.Status(UpdateStatus{})
	for _, is := range st.Integrations {
		if is.ID == IntegrationLuminar && is.Registered {
			t.Error("expected Registered=false when no syncer is wired")
		}
	}
}

func TestTriggerSyncSkipsConcurrentPassSameID(t *testing.T) {
	fs := &fakeIntegrationSyncer{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetIntegrationSyncers(map[IntegrationID]IntegrationSyncer{IntegrationLuminar: fs})

	done := make(chan struct{})
	go func() {
		r.TriggerSync(context.Background(), IntegrationLuminar)
		close(done)
	}()
	<-fs.started

	if _, ran := r.TriggerSync(context.Background(), IntegrationLuminar); ran {
		t.Error("expected a second TriggerSync for the SAME id to skip (ran=false) while a pass is already running")
	}

	close(fs.release)
	<-done
	if fs.calls != 1 {
		t.Errorf("expected exactly 1 Sync call, got %d", fs.calls)
	}
}

// TestTriggerSyncDifferentIDsDontBlockEachOther pins the deliberate
// per-ID (not single shared mutex) locking design: a pass in flight for
// one integration must never make a concurrent pass for a DIFFERENT
// integration report skipped.
func TestTriggerSyncDifferentIDsDontBlockEachOther(t *testing.T) {
	const otherID IntegrationID = "other-for-test"

	blocked := &fakeIntegrationSyncer{started: make(chan struct{}), release: make(chan struct{})}
	other := &fakeIntegrationSyncer{summary: SyncSummary{Emitted: 1}}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetIntegrationSyncers(map[IntegrationID]IntegrationSyncer{
		IntegrationLuminar: blocked,
		otherID:            other,
	})

	done := make(chan struct{})
	go func() {
		r.TriggerSync(context.Background(), IntegrationLuminar)
		close(done)
	}()
	<-blocked.started

	if _, ran := r.TriggerSync(context.Background(), otherID); !ran {
		t.Error("expected a concurrent sync for a DIFFERENT id to run, not be blocked by an in-flight pass for another id")
	}

	close(blocked.release)
	<-done
}

func TestTriggerSyncNeverBlocksOnIngestGate(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")
	fs := &fakeIntegrationSyncer{summary: SyncSummary{Emitted: 1}}
	r.SetIntegrationSyncers(map[IntegrationID]IntegrationSyncer{IntegrationLuminar: fs})

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	done := make(chan struct{})
	go func() {
		_, ran := r.TriggerSync(context.Background(), IntegrationLuminar)
		if !ran {
			t.Error("expected TriggerSync to run even while an ingest holds Runner.gate -- a sync uses its own locking, never gate")
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerSync appears to be blocked on Runner.gate")
	}

	close(release)
}

// TestTriggerIngestNeverBlocksOnSyncInFlight is the reverse of the above:
// a held sync must never block an ingest either -- neither direction
// shares a lock.
func TestTriggerIngestNeverBlocksOnSyncInFlight(t *testing.T) {
	fs := &fakeIntegrationSyncer{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetIntegrationSyncers(map[IntegrationID]IntegrationSyncer{IntegrationLuminar: fs})

	go r.TriggerSync(context.Background(), IntegrationLuminar)
	<-fs.started

	done := make(chan struct{})
	go func() {
		r.TriggerIngest(context.Background(), "/media/a")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerIngest appears to be blocked by an in-flight sync")
	}

	close(fs.release)
}

// fakeHookInstaller substitutes a real internal/resolvehook-backed
// installer for Runner tests -- same pattern as fakeIntegrationSyncer.
type fakeHookInstaller struct {
	state   HookState
	err     error
	started chan struct{}
	release chan struct{}
	calls   int

	revealCalls int
	revealErr   error
}

func (f *fakeHookInstaller) Install(_ context.Context) (HookState, error) {
	f.calls++
	if f.started != nil {
		close(f.started)
		<-f.release
	}
	return f.state, f.err
}

func (f *fakeHookInstaller) Reveal() error {
	f.revealCalls++
	return f.revealErr
}

func TestTriggerHookInstallRunsAndRecordsState(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fh := &fakeHookInstaller{state: HookState{Dir: "/scripts", Installed: true, UpToDate: true}}
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	state, ran := r.TriggerHookInstall(context.Background(), HookResolve)
	if !ran {
		t.Fatal("expected TriggerHookInstall to run when an installer is configured and idle")
	}
	if !state.Installed || !state.UpToDate {
		t.Errorf("got %+v", state)
	}
	if state.At.IsZero() {
		t.Error("expected TriggerHookInstall to stamp At, not leave it to the installer")
	}

	st := r.Status(UpdateStatus{})
	hs, ok := st.Hook(HookResolve)
	if !ok {
		t.Fatal("expected a HookStatus entry for HookResolve")
	}
	if !hs.Registered {
		t.Error("expected Registered=true once an installer is wired")
	}
	if hs.State == nil || !hs.State.Installed {
		t.Errorf("expected Status to reflect the install result, got %+v", hs.State)
	}
}

func TestTriggerHookInstallSkipsWhenNotRegistered(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, ran := r.TriggerHookInstall(context.Background(), HookResolve)
	if ran {
		t.Error("expected TriggerHookInstall to report ran=false when no installer is wired")
	}
}

func TestTriggerHookInstallSkipsConcurrentPass(t *testing.T) {
	fh := &fakeHookInstaller{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	done := make(chan struct{})
	go func() {
		r.TriggerHookInstall(context.Background(), HookResolve)
		close(done)
	}()
	<-fh.started

	if _, ran := r.TriggerHookInstall(context.Background(), HookResolve); ran {
		t.Error("expected a second TriggerHookInstall for the SAME id to skip (ran=false) while an install is already running")
	}

	close(fh.release)
	<-done
	if fh.calls != 1 {
		t.Errorf("expected exactly 1 Install call, got %d", fh.calls)
	}
}

// TestSetHookStateSeedsCacheWithoutCallingInstaller pins the
// cache-only, never-live-compute contract: SetHookState must be usable to
// seed the initial state (from a one-time startup resolvehook.Detect call)
// without ever touching a registered installer.
func TestSetHookStateSeedsCacheWithoutCallingInstaller(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fh := &fakeHookInstaller{}
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	r.SetHookState(HookResolve, HookState{Dir: "/scripts", Installed: false})

	st := r.Status(UpdateStatus{})
	hs, ok := st.Hook(HookResolve)
	if !ok || hs.State == nil {
		t.Fatal("expected SetHookState to be reflected in Status()")
	}
	if hs.State.Installed {
		t.Errorf("got %+v, want Installed=false as seeded", hs.State)
	}
	if fh.calls != 0 {
		t.Errorf("expected SetHookState to never call the installer, got %d calls", fh.calls)
	}
}

func TestStatusHooksOrderedByRegistry(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{})
	registry := Hooks()
	if len(st.Hooks) != len(registry) {
		t.Fatalf("got %d HookStatus entries, want %d (one per registry entry)", len(st.Hooks), len(registry))
	}
	for i, id := range registry {
		if st.Hooks[i].ID != id {
			t.Errorf("Hooks[%d].ID = %q, want %q (registry order)", i, st.Hooks[i].ID, id)
		}
		if st.Hooks[i].Registered {
			t.Errorf("Hooks[%d]: expected Registered=false with no installers wired", i)
		}
	}
}

func TestRevealHookCallsRegisteredInstaller(t *testing.T) {
	fh := &fakeHookInstaller{}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	if err := r.RevealHook(HookResolve); err != nil {
		t.Fatalf("RevealHook: %v", err)
	}
	if fh.revealCalls != 1 {
		t.Errorf("expected exactly 1 Reveal call, got %d", fh.revealCalls)
	}
}

func TestRevealHookReturnsInstallersError(t *testing.T) {
	fh := &fakeHookInstaller{revealErr: errors.New("xdg-open: not found")}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	if err := r.RevealHook(HookResolve); err == nil {
		t.Error("expected RevealHook to surface the installer's own error")
	}
}

func TestRevealHookErrorsWhenNotRegistered(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	if err := r.RevealHook(HookResolve); err == nil {
		t.Error("expected RevealHook to error when no installer is wired for this ID")
	}
}

// TestRevealHookNeverTouchesInstallInFlightOrCache pins RevealHook's own
// doc comment: Reveal must not participate in TriggerHookInstall's
// hookInFlight dedup (a concurrent Reveal must never be skipped) and must
// never write to the SetHookState cache (Status() must be unaffected).
func TestRevealHookNeverTouchesInstallInFlightOrCache(t *testing.T) {
	fh := &fakeHookInstaller{state: HookState{Dir: "/scripts", Installed: true, UpToDate: true}}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})
	r.SetHookState(HookResolve, HookState{Dir: "/seeded", Installed: false})

	if err := r.RevealHook(HookResolve); err != nil {
		t.Fatalf("RevealHook: %v", err)
	}
	if _, ran := r.TriggerHookInstall(context.Background(), HookResolve); !ran {
		t.Error("a prior RevealHook call must not block a subsequent TriggerHookInstall via hookInFlight")
	}

	st, ok := r.Status(UpdateStatus{}).Hook(HookResolve)
	if !ok || st.State == nil || st.State.Dir != "/scripts" {
		t.Errorf("expected TriggerHookInstall's own state to have overwritten the seeded state, got %+v -- RevealHook must never write the cache itself", st.State)
	}
}

func TestStatusIntegrationsOrderedByRegistry(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{})
	registry := Integrations()
	if len(st.Integrations) != len(registry) {
		t.Fatalf("got %d IntegrationStatus entries, want %d (one per registry entry)", len(st.Integrations), len(registry))
	}
	for i, d := range registry {
		if st.Integrations[i].ID != d.ID {
			t.Errorf("Integrations[%d].ID = %q, want %q (registry order)", i, st.Integrations[i].ID, d.ID)
		}
		if st.Integrations[i].Registered {
			t.Errorf("Integrations[%d]: expected Registered=false with no syncers wired", i)
		}
	}
}

func TestStatusScratchNote(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	if got := r.Status(UpdateStatus{}).ScratchNote; got != "not configured" {
		t.Errorf("got %q, want 'not configured' for an empty ScratchDir", got)
	}

	r2 := NewRunner(&fakeIngester{}, nil, "/local/scratch")
	got := r2.Status(UpdateStatus{}).ScratchNote
	if !strings.Contains(got, "/local/scratch") || !strings.Contains(got, "not yet implemented") {
		t.Errorf("got %q, want the configured path plus an explicit not-yet-implemented note (never a fabricated usage number)", got)
	}
}

func TestStatusWatchDirsIsACopy(t *testing.T) {
	dirs := []string{"/media/a"}
	r := NewRunner(&fakeIngester{}, dirs, "")
	st := r.Status(UpdateStatus{})
	st.WatchDirs[0] = "mutated"
	if got := r.WatchDirs(); got[0] != "/media/a" {
		t.Error("Status() must return a copy of WatchDirs, not the live slice")
	}
}

func TestStatusSurfacesBusySince(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	st := r.Status(UpdateStatus{})
	if !st.Busy {
		t.Fatal("expected Busy=true during ingest")
	}
	if st.BusySince.IsZero() {
		t.Error("expected BusySince to be set during ingest")
	}

	close(release)
}

func TestStatusSurfacesHandshakeOK(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true}}
	r.SetQueueDeps(nil, fd, nil)

	r.TriggerDrain(context.Background())

	st := r.Status(UpdateStatus{})
	if !st.HandshakeOK {
		t.Error("expected HandshakeOK=true when last drain succeeded with handshake")
	}
}

func TestStatusSurfacesHandshakeNOTOK(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
	r.SetQueueDeps(nil, fd, nil)

	r.TriggerDrain(context.Background())

	st := r.Status(UpdateStatus{})
	if st.HandshakeOK {
		t.Error("expected HandshakeOK=false when last drain had failed handshake")
	}
}

func TestStatusHandshakeOKFalseWithNoDrains(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{})
	if st.HandshakeOK {
		t.Error("expected HandshakeOK=false when no drains have run yet")
	}
}

func TestStatusSurfacesInFlightDrain(t *testing.T) {
	fd := &fakeDrainer{started: make(chan struct{}), release: make(chan struct{})}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetQueueDeps(nil, fd, nil)

	done := make(chan struct{})
	go func() {
		r.TriggerDrain(context.Background())
		close(done)
	}()
	<-fd.started

	st := r.Status(UpdateStatus{})
	if !st.InFlightDrain {
		t.Error("expected InFlightDrain=true while a drain pass is running")
	}

	close(fd.release)
	<-done

	st = r.Status(UpdateStatus{})
	if st.InFlightDrain {
		t.Error("expected InFlightDrain=false once drain pass completes")
	}
}

// TestTriggerDrainInFlightDrainStaysFalseWhenNotConfigured is the
// regression test for the Hermes review on #123: the periodic drain
// timer (cmd/branchdam-agent/tray.go:214) runs unconditionally, so on
// an unconfigured queue (no drainer wired) a TriggerDrain tick must
// leave Status().InFlightDrain strictly false. The earlier
// implementation set the flag before the nil-drainer guard and
// reset it via defer, briefly flashing "drain in progress..." to a
// concurrent Status() call on every tick.
func TestTriggerDrainInFlightDrainStaysFalseWhenNotConfigured(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	// Deliberately do NOT call SetQueueDeps with a drainer: this
	// mirrors the "unconfigured offline queue" case.

	for i := 0; i < 5; i++ {
		_, ran := r.TriggerDrain(context.Background())
		if ran {
			t.Fatalf("tick %d: expected ran=false on a Runner with no drainer wired", i)
		}
		st := r.Status(UpdateStatus{})
		if st.InFlightDrain {
			t.Fatalf("tick %d: InFlightDrain must stay false on a no-drainer Runner, got true (status=%+v)", i, st)
		}
	}
}

// TestTriggerPruneInFlightPruneStaysFalseWhenNotConfigured is the
// prune-side mirror of TestTriggerDrainInFlightDrainStaysFalseWhenNotConfigured:
// a menu-click TriggerPrune on an unconfigured pruner must leave
// Status().InFlightPrune strictly false. TriggerPrune is invoked
// from the tray's "Prune now" menu item (run_supported.go) and would
// otherwise briefly flash "prune in progress..." on a Runner with no
// pruner wired.
func TestTriggerPruneInFlightPruneStaysFalseWhenNotConfigured(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	// Deliberately do NOT call SetQueueDeps with a pruner: this
	// mirrors the "unconfigured pruner" case (prune.enabled=false).

	for i := 0; i < 5; i++ {
		_, ran := r.TriggerPrune(context.Background())
		if ran {
			t.Fatalf("tick %d: expected ran=false on a Runner with no pruner wired", i)
		}
		st := r.Status(UpdateStatus{})
		if st.InFlightPrune {
			t.Fatalf("tick %d: InFlightPrune must stay false on a no-pruner Runner, got true (status=%+v)", i, st)
		}
	}
}

func TestStatusInFlightPruneFalseWhenIdle(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{})
	if st.InFlightPrune {
		t.Error("expected InFlightPrune=false when nothing is running")
	}
}

func TestTriggerIngestSerializesConcurrentCalls(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	if _, _, busy := r.Busy(); !busy {
		t.Fatal("expected Busy() to report true while an ingest is in flight")
	}
	if _, ok := r.TryLockIdle(); ok {
		t.Fatal("expected TryLockIdle to fail while an ingest is in flight")
	}

	done := make(chan struct{})
	go func() {
		r.TriggerIngest(context.Background(), "/media/b")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("second TriggerIngest returned before the first released -- calls are not serialized")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	<-done

	if _, _, busy := r.Busy(); busy {
		t.Error("expected Busy() to report false once both ingests finished")
	}
	if release, ok := r.TryLockIdle(); !ok {
		t.Error("expected TryLockIdle to succeed once idle")
	} else {
		release()
	}
}

func TestReconfigureSwapsIngesterWatchDirsAndScratch(t *testing.T) {
	r := NewRunner(&fakeIngester{}, []string{"/old"}, "/old-scratch")

	newIngester := &fakeIngester{}
	r.Reconfigure(newIngester, []string{"/new-a", "/new-b"}, "/new-scratch")

	if got := r.WatchDirs(); len(got) != 2 || got[0] != "/new-a" || got[1] != "/new-b" {
		t.Errorf("WatchDirs() after Reconfigure = %v, want [/new-a /new-b]", got)
	}
	st := r.Status(UpdateStatus{})
	if !strings.Contains(st.ScratchNote, "/new-scratch") {
		t.Errorf("ScratchNote after Reconfigure = %q, want it to mention /new-scratch", st.ScratchNote)
	}

	r.TriggerIngest(context.Background(), "/new-a")
	if len(newIngester.calls) != 1 || newIngester.calls[0] != "/new-a" {
		t.Errorf("expected TriggerIngest to call the reconfigured ingester, got calls=%v", newIngester.calls)
	}
}

func TestReconfigureWaitsForInFlightIngest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, []string{"/old"}, "")

	go r.TriggerIngest(context.Background(), "/old")
	<-started

	done := make(chan struct{})
	go func() {
		r.Reconfigure(&fakeIngester{}, []string{"/new"}, "")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Reconfigure returned while an ingest was still in flight -- it must block on the same gate TriggerIngest holds")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	<-done

	if got := r.WatchDirs(); len(got) != 1 || got[0] != "/new" {
		t.Errorf("WatchDirs() after Reconfigure = %v, want [/new]", got)
	}
}

func TestReconfigureDetectorStartsAndStops(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{t.TempDir()})

	r.detectorMu.Lock()
	cancelFn := r.detectorCancel
	doneCh := r.detectorDone
	r.detectorMu.Unlock()

	if cancelFn == nil || doneCh == nil {
		t.Fatal("expected detectorCancel and detectorDone to be non-nil when roots are configured")
	}

	// Reconfigure with empty roots stops the detector
	r.ReconfigureDetector(context.Background(), nil)

	select {
	case <-doneCh:
		// success: old goroutine exited
	case <-time.After(500 * time.Millisecond):
		t.Fatal("old detector goroutine did not exit after ReconfigureDetector(context.Background(), nil)")
	}

	r.detectorMu.Lock()
	cancelFn = r.detectorCancel
	doneCh = r.detectorDone
	r.detectorMu.Unlock()

	if cancelFn != nil || doneCh != nil {
		t.Error("expected detectorCancel and detectorDone to be nil after clearing roots")
	}
}

func TestReconfigureDetectorWaitsForPreviousGoroutine(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{t.TempDir()})

	r.detectorMu.Lock()
	firstDoneCh := r.detectorDone
	r.detectorMu.Unlock()

	// Reconfigure with new roots replaces the detector and waits for previous one
	r.ReconfigureDetector(ctx, []string{t.TempDir()})

	select {
	case <-firstDoneCh:
		// success
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first detector goroutine did not exit when replaced")
	}

	r.StopDetector()
}

func TestReconfigureTriggersReconfigureDetectorOnRootsChange(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/old"}, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{"/old"})

	r.detectorMu.Lock()
	firstDoneCh := r.detectorDone
	r.detectorMu.Unlock()

	// Reconfigure with changed roots
	r.Reconfigure(fi, []string{"/new"}, "")

	select {
	case <-firstDoneCh:
		// success: old goroutine exited
	case <-time.After(500 * time.Millisecond):
		t.Fatal("old detector goroutine did not exit when Reconfigure changed watchDirs")
	}

	if got := r.WatchDirs(); len(got) != 1 || got[0] != "/new" {
		t.Errorf("WatchDirs() = %v, want [/new]", got)
	}

	r.StopDetector()
}

func TestReconfigureNoDeadlockWithInFlightIngest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, []string{"/old"}, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{"/old"})

	// Start an ingest that holds gate
	go r.TriggerIngest(context.Background(), "/old/card")
	<-started

	// Reconfigure with changed roots while ingest is in flight
	done := make(chan struct{})
	go func() {
		r.Reconfigure(fi, []string{"/new"}, "")
		close(done)
	}()

	// Release the in-flight ingest
	close(release)

	select {
	case <-done:
		// success: Reconfigure completed without deadlock
	case <-time.After(2 * time.Second):
		t.Fatal("Reconfigure deadlocked with in-flight ingest")
	}

	r.StopDetector()
}

func TestSetDetectorIntervalAndBaseContext(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetDetectorInterval(0)
	if r.detectorInterval != ingest.DefaultPollInterval {
		t.Errorf("detectorInterval = %v, want default %v", r.detectorInterval, ingest.DefaultPollInterval)
	}

	r.SetDetectorInterval(-5 * time.Second)
	if r.detectorInterval != ingest.DefaultPollInterval {
		t.Errorf("detectorInterval = %v, want default %v", r.detectorInterval, ingest.DefaultPollInterval)
	}

	r.SetDetectorInterval(500 * time.Millisecond)
	if r.detectorInterval != 500*time.Millisecond {
		t.Errorf("detectorInterval = %v, want 500ms", r.detectorInterval)
	}

	if r.BaseContext() != context.Background() {
		t.Errorf("BaseContext() when unset = %v, want context.Background()", r.BaseContext())
	}

	customCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.detectorBaseCtx = customCtx
	if r.BaseContext() != customCtx {
		t.Errorf("BaseContext() = %v, want customCtx", r.BaseContext())
	}
}

func TestReconfigureDetectorInvokesOnCardIngested(t *testing.T) {
	fi := &fakeIngester{}
	dir := t.TempDir()
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	var hit int32
	r.SetOnCardIngested(func() {
		atomic.AddInt32(&hit, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{dir})

	// Create a subdirectory to trigger detector
	cardDir := filepath.Join(dir, "CARD1")
	if err := os.Mkdir(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		if atomic.LoadInt32(&hit) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&hit) == 0 {
		t.Error("expected onCardIngested callback to be invoked after card detection")
	}

	r.StopDetector()
}

func TestReconfigureSameRootsDoesNotRestartDetector(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/same"}, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{"/same"})

	r.detectorMu.Lock()
	firstDoneCh := r.detectorDone
	r.detectorMu.Unlock()

	// Reconfigure with identical roots
	r.Reconfigure(fi, []string{"/same"}, "/new-scratch")

	r.detectorMu.Lock()
	secondDoneCh := r.detectorDone
	r.detectorMu.Unlock()

	if firstDoneCh != secondDoneCh {
		t.Error("expected Reconfigure with same roots to preserve running detector goroutine")
	}

	r.StopDetector()
}

func TestReconfigureDetectorErrorHandler(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")

	var gotErr error
	r.SetDetectorErrorHandler(func(err error) {
		gotErr = err
	})

	testErr := errors.New("detector test error")
	r.mu.Lock()
	handler := r.detectorErrHandler
	r.mu.Unlock()

	if handler == nil {
		t.Fatal("expected detectorErrHandler to be set")
	}
	handler(testErr)
	if gotErr != testErr {
		t.Errorf("gotErr = %v, want %v", gotErr, testErr)
	}

	r.SetDetectorErrorHandler(nil)
	r.mu.Lock()
	handler = r.detectorErrHandler
	r.mu.Unlock()
	if handler != nil {
		t.Error("expected detectorErrHandler to be nil after clearing")
	}
}

// fakeSelfUpdater is a no-op SelfUpdater shared by tests across build
// tags (run_unsupported_test.go's Linux stub test and, eventually,
// run_supported_test.go's windows/darwin ones).
type fakeSelfUpdater struct{}

func (fakeSelfUpdater) Status() UpdateStatus                          { return UpdateStatus{} }
func (fakeSelfUpdater) ApplyLatest(_ context.Context) (string, error) { return "", nil }
func (fakeSelfUpdater) RollbackAvailable() (string, bool)             { return "", false }
func (fakeSelfUpdater) Rollback(_ context.Context) (string, error)    { return "", nil }

// fakeSettings is a no-op Settings shared by tests across build tags, the
// same way fakeSelfUpdater is.
type fakeSettings struct{}

func (fakeSettings) Snapshot() SettingsView                     { return SettingsView{} }
func (fakeSettings) SetBool(_ string, _ bool) error             { return nil }
func (fakeSettings) SetInt(_ string, _ int) error               { return nil }
func (fakeSettings) PromptAndSet(_ SettingsField) (bool, error) { return false, nil }
func (fakeSettings) PromptAndSetIntegrationPath(_ IntegrationID) (bool, error) {
	return false, nil
}
func (fakeSettings) Reload() error             { return nil }
func (fakeSettings) OpenConfigFile() error     { return nil }
func (fakeSettings) RevealConfigFolder() error { return nil }

type blockingIngester struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingIngester) IngestCard(_ context.Context, _ string) (ingest.CardResult, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return ingest.CardResult{}, nil
}

func (b *blockingIngester) IngestCardOffline(_ context.Context, _ string) (ingest.OfflineCardResult, error) {
	return ingest.OfflineCardResult{}, nil
}

func TestSetDetectorRequireDCIM(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")
	r.SetDetectorRequireDCIM(true)
	if !r.detectorRequireDCIM {
		t.Error("expected detectorRequireDCIM to be true")
	}
	r.SetDetectorRequireDCIM(false)
	if r.detectorRequireDCIM {
		t.Error("expected detectorRequireDCIM to be false")
	}
}

func TestReconfigureDetectorWithRequireDCIM(t *testing.T) {
	fi := &fakeIngester{}
	dir := t.TempDir()
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)
	r.SetDetectorRequireDCIM(true)

	var hit int32
	r.SetOnCardIngested(func() {
		atomic.AddInt32(&hit, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{dir})

	// Create a subdirectory without DCIM -> should not trigger ingest
	usbDir := filepath.Join(dir, "USB_STICK")
	if err := os.Mkdir(usbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&hit) > 0 {
		t.Error("expected USB stick without DCIM not to trigger ingest when requireDCIM=true")
	}

	// Create DCIM directory -> should trigger ingest
	if err := os.Mkdir(filepath.Join(usbDir, "DCIM"), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		if atomic.LoadInt32(&hit) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&hit) == 0 {
		t.Error("expected camera card with DCIM to trigger ingest when requireDCIM=true")
	}

	r.StopDetector()
}
