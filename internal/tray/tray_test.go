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
	if st.LastIngest == nil || !st.LastIngest.Offline {
		t.Error("expected st.LastIngest.Offline=true")
	}
	if st.QueueStatus.Counts.Pending() != 2 {
		t.Errorf("expected QueueStatus.Pending=2, got %d", st.QueueStatus.Counts.Pending())
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
	if notifiedTitle != "branchDAM Ingest" {
		t.Errorf("expected notifyError title %q, got %q", "branchDAM Ingest", notifiedTitle)
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
	if st.LastIngest == nil || st.LastIngest.Offline {
		t.Error("expected st.LastIngest.Offline=false")
	}
}

func TestTriggerIngestProberTrueButIngestFails(t *testing.T) {
	wantErr := errors.New("copy failed")
	fi := &fakeIngester{err: wantErr}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")
	r.SetArchiveRoot("/nas/archive")
	r.SetArchiveProber(func(ctx context.Context, archiveRoot string) bool {
		return true
	})
	r.SetQueueDeps(&fakeQueueReader{}, nil, nil)

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if !errors.Is(summary.Err, wantErr) {
		t.Fatalf("summary.Err = %v, want %v", summary.Err, wantErr)
	}
	if summary.Offline {
		t.Error("expected prober=true to keep the ingest online even when IngestCard fails")
	}
	if len(fi.calls) != 1 {
		t.Errorf("expected one online ingest call, got %d", len(fi.calls))
	}
	if len(fi.offlineCalls) != 0 {
		t.Errorf("expected no offline fallback after a reachable probe, got %d calls", len(fi.offlineCalls))
	}
}

func TestTriggerIngestOfflineFallbackUsesOfflineNotification(t *testing.T) {
	fi := &fakeIngester{
		offlineResult: ingest.OfflineCardResult{
			Files: []ingest.OfflineFileResult{{SourcePath: "a.jpg", LocalPath: "/scratch/a.jpg"}},
		},
	}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")
	r.SetArchiveProber(func(context.Context, string) bool { return false })
	r.SetQueueDeps(&fakeQueueReader{}, nil, nil)
	var notification string
	r.SetNotifier(func(_, message string) {
		notification = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/card")

	if !summary.Offline {
		t.Fatal("expected offline summary")
	}
	if notification != "1 photo queued offline from card" {
		t.Fatalf("notification = %q, want offline wording", notification)
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

// TestRefreshHookStateOverwritesSeedAndDoesNotCallInstaller pins the
// settings-reload invalidation contract for issue #154: when the operator
// edits integrations.resolve.scriptsDir (or any other field the hook
// installer reads) and the cmd/branchdam-agent reload path re-runs
// resolvehook.Detect against the new config, the resulting snapshot must
// overwrite the prior cached state in Runner.hookState. RefreshHookState
// is a direct cache writer like SetHookState -- it MUST NOT call the
// registered installer (caller already did the Detect), and it MUST NOT
// race a concurrent in-flight TriggerHookInstall (so the install's own
// post-completion cache write wins, not a stale post-reload snapshot
// overwriting a fresh install result).
func TestRefreshHookStateOverwritesSeedAndDoesNotCallInstaller(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fh := &fakeHookInstaller{}
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	r.SetHookState(HookResolve, HookState{At: time.Now(), Dir: "/old/scripts", Installed: true, UpToDate: true})
	r.RefreshHookState(HookResolve, HookState{Dir: "/new/scripts", Installed: false})

	st := r.Status(UpdateStatus{})
	hs, ok := st.Hook(HookResolve)
	if !ok || hs.State == nil {
		t.Fatal("expected RefreshHookState to be reflected in Status()")
	}
	if hs.State.Dir != "/new/scripts" {
		t.Errorf("got Dir=%q, want %q -- RefreshHookState must overwrite the prior seeded state", hs.State.Dir, "/new/scripts")
	}
	if hs.State.Installed {
		t.Errorf("got Installed=true, want false from the refresh snapshot")
	}
	if fh.calls != 0 {
		t.Errorf("expected RefreshHookState to never call the installer, got %d calls", fh.calls)
	}
}

// TestRefreshHookStateAfterInstallWins pins the race the AC explicitly
// calls out: a TriggerHookInstall that completes AFTER a reload-path
// RefreshHookState (because the install's I/O was slower than the reload)
// must overwrite the refresh snapshot with its own post-install result.
// Otherwise a user who clicks "Install" right after editing a setting
// would see "not installed" until the next startup.
//
// This is a real concurrency test, not a sequential one: the install
// runs in its own goroutine, blocks on fakeHookInstaller's `started`/
// `release` channels, and the refresh call is timed to land in the
// correct order relative to the install's lock-release. With r.mu
// serializing both writes, the write that grabs the lock LAST is the
// one that survives -- so the assertion is exactly that, for this
// ordering, the install's post-Install write wins over the earlier
// refresh write.
func TestRefreshHookStateAfterInstallWins(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fh := &fakeHookInstaller{state: HookState{Dir: "/installed", Installed: true, UpToDate: true}, started: started, release: release}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	// Kick off the install in a goroutine. It will close `started` (to
	// signal it has entered Install and is blocked on `release`) and
	// then wait. The test holds the install open here and decides when
	// to release it, so the interleaving with RefreshHookState is
	// deterministic.
	installDone := make(chan struct{})
	go func() {
		defer close(installDone)
		_, ran := r.TriggerHookInstall(context.Background(), HookResolve)
		if !ran {
			t.Errorf("expected the install to run")
		}
	}()

	// Wait until the install is past its hookInFlight bookkeeping and
	// blocked inside Install (i.e. has released r.mu after the inflight
	// flag was set). After this, RefreshHookState issued by the test
	// goroutine is guaranteed to acquire r.mu before the install
	// completes, so the refresh write lands FIRST and the install's
	// own post-Install write (which re-takes r.mu) lands LAST and
	// wins.
	<-started

	r.RefreshHookState(HookResolve, HookState{Dir: "/new/scripts", Installed: false})
	close(release)
	<-installDone

	st := r.Status(UpdateStatus{})
	hs, ok := st.Hook(HookResolve)
	if !ok || hs.State == nil {
		t.Fatal("expected install result to be reflected in Status()")
	}
	if hs.State.Dir != "/installed" {
		t.Errorf("got Dir=%q, want %q -- a TriggerHookInstall that completes after RefreshHookState must overwrite the refresh snapshot (real concurrency, not sequential)", hs.State.Dir, "/installed")
	}
	if !hs.State.Installed || !hs.State.UpToDate {
		t.Errorf("got %+v, want Installed=true UpToDate=true from the install result", hs.State)
	}
}

// TestRefreshHookStateAfterCompletedInstallWins is the reverse-ordering
// pin: an install that has FULLY COMPLETED, then a reload-path
// RefreshHookState fires, then the operator's view should reflect the
// fresh reload snapshot, not the install's earlier result. The other
// direction (a refresh lands between the install's hookInFlight set and
// its post-Install write) is covered by TestRefreshHookStateAfterInstallWins
// above; this one pins the simpler "install returned, refresh races
// nothing, refresh's write is the last one" path -- a regression guard
// against a future refactor that accidentally reorders RefreshHookState
// to read-then-write without holding r.mu.
func TestRefreshHookStateAfterCompletedInstallWins(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fh := &fakeHookInstaller{state: HookState{Dir: "/installed", Installed: true, UpToDate: true}}
	r.SetHookInstallers(map[HookID]HookInstaller{HookResolve: fh})

	// Install completes synchronously (no started/release wiring).
	if _, ran := r.TriggerHookInstall(context.Background(), HookResolve); !ran {
		t.Fatal("expected the install to run")
	}

	// Sanity: the install's result is the current cache.
	if got := r.Status(UpdateStatus{}).Hooks[0].State.Dir; got != "/installed" {
		t.Fatalf("post-install Dir = %q, want %q", got, "/installed")
	}

	// Now the reload path fires its RefreshHookState -- the operator
	// just edited scriptsDir. The cache must surface the new snapshot.
	r.RefreshHookState(HookResolve, HookState{Dir: "/new", Installed: false})

	st := r.Status(UpdateStatus{})
	hs, ok := st.Hook(HookResolve)
	if !ok || hs.State == nil {
		t.Fatal("expected RefreshHookState to be reflected in Status()")
	}
	if hs.State.Dir != "/new" {
		t.Errorf("got Dir=%q, want %q -- a RefreshHookState that lands after a completed install must overwrite the install's result", hs.State.Dir, "/new")
	}
	if hs.State.Installed {
		t.Errorf("got Installed=true, want false from the refresh snapshot")
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

func TestTriggerIngestProbeDoesNotHoldGate(t *testing.T) {
	started := make(chan struct{})
	releaseProbe := make(chan struct{})
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetArchiveProber(func(context.Context, string) bool {
		close(started)
		<-releaseProbe
		return true
	})

	done := make(chan IngestSummary, 1)
	go func() {
		done <- r.TriggerIngest(context.Background(), "/media/card")
	}()
	<-started

	releaseGate, ok := r.TryLockIdle()
	if !ok {
		close(releaseProbe)
		<-done
		t.Fatal("reachability probe held Runner.gate")
	}
	releaseGate()
	close(releaseProbe)
	<-done
}

func TestTriggerIngestStartsClockAfterGate(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	releaseGate, ok := r.TryLockIdle()
	if !ok {
		t.Fatal("expected to acquire idle gate")
	}

	done := make(chan IngestSummary, 1)
	go func() {
		done <- r.TriggerIngest(context.Background(), "/media/card")
	}()
	time.Sleep(100 * time.Millisecond)
	releaseGate()

	summary := <-done
	if summary.Elapsed >= 50*time.Millisecond {
		t.Fatalf("ingest elapsed time %s includes gate-wait time", summary.Elapsed)
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
func (fakeSettings) SetStringSlice(_ string, _ []string) error  { return nil }
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

type fakeIngestGate struct {
	calls   []string
	proceed bool
	err     error
}

func (f *fakeIngestGate) Confirm(_ context.Context, volumePath, volumeName string) (bool, error) {
	f.calls = append(f.calls, volumePath)
	return f.proceed, f.err
}

func TestTriggerIngestWithGate(t *testing.T) {
	fi := &fakeIngester{
		result: ingest.CardResult{
			Files: []ingest.FileResult{
				{SourcePath: "IMG_0001.JPG"},
			},
		},
	}
	r := NewRunner(fi, nil, "")

	// 1. Nil gate -> proceeds unconditionally
	summary := r.TriggerDetectedIngest(context.Background(), "/media/card1")
	if !summary.OK() || summary.Submitted != 1 {
		t.Fatalf("expected submitted=1, got summary=%+v", summary)
	}

	// 2. Gate returning error (transient failure, e.g. render failed)
	gate := &fakeIngestGate{proceed: false, err: errors.New("zenity failed")}
	r.SetIngestGate(gate)

	summary = r.TriggerDetectedIngest(context.Background(), "/media/card2")
	if summary.Err == nil {
		t.Fatal("expected summary.Err to be non-nil on gate error")
	}
	if summary.Submitted != 0 {
		t.Fatalf("expected 0 submitted on error, got %+v", summary)
	}
	if r.IsSkipped("/media/card2") {
		t.Fatal("expected /media/card2 NOT to be in skipped set on error")
	}

	// Manual TriggerIngest bypasses gate and proceeds unconditionally
	manualSummary := r.TriggerIngest(context.Background(), "/media/card2")
	if !manualSummary.OK() || manualSummary.Submitted != 1 {
		t.Fatalf("expected manual TriggerIngest to proceed unconditionally, got %+v", manualSummary)
	}
	if len(gate.calls) != 1 {
		t.Fatalf("expected manual TriggerIngest NOT to call gate, calls=%v", gate.calls)
	}

	// Next detection call on same path triggers gate again because it was not added to skipped set
	gate.err = nil
	gate.proceed = false
	gate.calls = nil

	// 3. Gate returning false (explicit "Skip this time")
	summary = r.TriggerDetectedIngest(context.Background(), "/media/card2")
	if summary.Err != nil {
		t.Fatalf("unexpected summary.Err: %v", summary.Err)
	}
	if summary.Submitted != 0 {
		t.Fatalf("expected ingest skipped, got %+v", summary)
	}
	if len(gate.calls) != 1 || gate.calls[0] != "/media/card2" {
		t.Fatalf("expected gate called for /media/card2, got %v", gate.calls)
	}
	if !r.IsSkipped("/media/card2") {
		t.Fatal("expected /media/card2 in skipped set")
	}

	// 4. Second detection call on the same path suppresses dialog (session-scoped skip)
	summary = r.TriggerDetectedIngest(context.Background(), "/media/card2")
	if summary.Submitted != 0 {
		t.Fatalf("expected ingest skipped, got %+v", summary)
	}
	if len(gate.calls) != 1 {
		t.Fatalf("expected gate NOT called again for skipped path, got calls=%v", gate.calls)
	}

	// Manual TriggerIngest still proceeds even when path is in skipped set
	manualSummary = r.TriggerIngest(context.Background(), "/media/card2")
	if !manualSummary.OK() || manualSummary.Submitted != 1 {
		t.Fatalf("expected manual TriggerIngest to proceed even when path is in skipped set, got %+v", manualSummary)
	}

	// 5. ForgetSkipped clears skip set
	r.ForgetSkipped("/media/card2")
	if r.IsSkipped("/media/card2") {
		t.Fatal("expected /media/card2 removed from skipped set")
	}

	// 6. Next detection call triggers gate again
	gate.proceed = true
	summary = r.TriggerDetectedIngest(context.Background(), "/media/card2")
	if !summary.OK() || summary.Submitted != 1 {
		t.Fatalf("expected submitted=1 after un-skipping, got summary=%+v", summary)
	}
	if len(gate.calls) != 2 {
		t.Fatalf("expected gate called again after un-skipping, got calls=%v", gate.calls)
	}
}

func TestTriggerIngestNotification(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")

	var notifications []struct{ title, message string }
	r.SetNotifier(func(title, message string) {
		notifications = append(notifications, struct{ title, message string }{title, message})
	})

	// Plural: 8 photos
	fi.result = ingest.CardResult{
		Files: make([]ingest.FileResult, 8),
	}
	r.TriggerIngest(context.Background(), "/Volumes/CANON R5")
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	if notifications[0].message != "8 photos imported from CANON R5" {
		t.Errorf("got msg %q, want %q", notifications[0].message, "8 photos imported from CANON R5")
	}

	// Singular: 1 photo
	notifications = nil
	fi.result = ingest.CardResult{
		Files: make([]ingest.FileResult, 1),
	}
	r.TriggerIngest(context.Background(), "/Volumes/CANON R5")
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	if notifications[0].message != "1 photo imported from CANON R5" {
		t.Errorf("got msg %q, want %q", notifications[0].message, "1 photo imported from CANON R5")
	}

	// Error: no notification
	notifications = nil
	fi.err = errors.New("read failed")
	r.TriggerIngest(context.Background(), "/Volumes/CANON R5")
	if len(notifications) != 0 {
		t.Errorf("expected no notification on error, got %v", notifications)
	}
}

func TestDetectorWatchForgetsSkippedOnRemoval(t *testing.T) {
	fi := &fakeIngester{
		result: ingest.CardResult{
			Files: []ingest.FileResult{{SourcePath: "IMG_0001.JPG"}},
		},
	}
	dir := t.TempDir()
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)

	gate := &fakeIngestGate{proceed: false}
	r.SetIngestGate(gate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{dir})

	cardDir := filepath.Join(dir, "CARD1")
	if err := os.Mkdir(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Wait for card to be detected and skipped
	for i := 0; i < 50; i++ {
		if r.IsSkipped(cardDir) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !r.IsSkipped(cardDir) {
		t.Fatal("expected cardDir in skipped set")
	}

	// Remove volume directory
	if err := os.Remove(cardDir); err != nil {
		t.Fatal(err)
	}

	// Wait for detector to observe removal and call ForgetSkipped
	for i := 0; i < 50; i++ {
		if !r.IsSkipped(cardDir) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.IsSkipped(cardDir) {
		t.Fatal("expected cardDir removed from skipped set after unmount")
	}

	r.StopDetector()
}

func TestRunnerPauseDefaultsToFalse(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	if r.Paused() {
		t.Error("expected Paused() to default to false")
	}
	st := r.Status(UpdateStatus{})
	if st.Paused {
		t.Error("expected Status().Paused to default to false")
	}
}

func TestRunnerSetPausedAndCallback(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	var gotState bool
	var called int
	r.SetOnPauseChange(func(paused bool) {
		gotState = paused
		called++
	})

	r.SetPaused(true)
	if !r.Paused() {
		t.Error("expected Paused() to be true after SetPaused(true)")
	}
	if called != 1 || !gotState {
		t.Errorf("callback called=%d, gotState=%v, want 1, true", called, gotState)
	}

	r.SetPaused(false)
	if r.Paused() {
		t.Error("expected Paused() to be false after SetPaused(false)")
	}
	if called != 2 || gotState {
		t.Errorf("callback called=%d, gotState=%v, want 2, false", called, gotState)
	}
}

func TestTriggerIngestSkipsWhenPaused(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{{SourcePath: "a.jpg"}}}}
	r := NewRunner(fi, nil, "")
	r.SetPaused(true)

	summary := r.TriggerIngest(context.Background(), "/media/card")
	if summary.CardPath != "/media/card" {
		t.Errorf("summary.CardPath = %q, want /media/card", summary.CardPath)
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected 0 IngestCard calls when paused, got %d", len(fi.calls))
	}
	if summary.Submitted != 0 || summary.Failed != 0 {
		t.Errorf("got %+v, expected 0 submitted/failed", summary)
	}

	st := r.Status(UpdateStatus{})
	if st.LastIngest != nil {
		t.Errorf("expected LastIngest to be nil after skipped TriggerIngest, got %+v", st.LastIngest)
	}
}

func TestTriggerDrainSkipsWhenPaused(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 2}}
	r.SetQueueDeps(nil, fd, nil)

	r.SetPaused(true)
	summary, ran := r.TriggerDrain(context.Background())
	if ran {
		t.Error("expected TriggerDrain to return ran=false when paused")
	}
	if summary != (DrainSummary{}) {
		t.Errorf("expected empty DrainSummary when paused, got %+v", summary)
	}
	if fd.calls != 0 {
		t.Errorf("expected 0 Drain calls when paused, got %d", fd.calls)
	}

	// When resumed, TriggerDrain works normally
	r.SetPaused(false)
	summary, ran = r.TriggerDrain(context.Background())
	if !ran || summary.NodeCreatedSent != 2 {
		t.Errorf("expected TriggerDrain to run after resume, got ran=%v, summary=%+v", ran, summary)
	}
	if fd.calls != 1 {
		t.Errorf("expected 1 Drain call, got %d", fd.calls)
	}
}

func TestTriggerDrainPauseOnMetered(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 2}}
	r.SetQueueDeps(nil, fd, nil)

	// Case 1: PauseUploadOnMetered=true and isMetered=true -> drain skipped
	r.SetPauseUploadOnMetered(true)
	r.SetIsMeteredFunc(func() (bool, error) { return true, nil })

	summary, ran := r.TriggerDrain(context.Background())
	if ran {
		t.Error("expected TriggerDrain to return ran=false on metered network when PauseUploadOnMetered is true")
	}
	if summary.NodeCreatedSent != 0 {
		t.Errorf("expected zero summary, got %+v", summary)
	}
	if fd.calls != 0 {
		t.Errorf("expected 0 Drain calls, got %d", fd.calls)
	}

	// Case 2: PauseUploadOnMetered=true and isMetered=false (unmetered) -> drain proceeds
	r.SetIsMeteredFunc(func() (bool, error) { return false, nil })
	summary, ran = r.TriggerDrain(context.Background())
	if !ran {
		t.Error("expected TriggerDrain to return ran=true on unmetered network")
	}
	if summary.NodeCreatedSent != 2 {
		t.Errorf("expected NodeCreatedSent=2, got %+v", summary)
	}
	if fd.calls != 1 {
		t.Errorf("expected 1 Drain call, got %d", fd.calls)
	}
}

func TestTriggerPruneSkipsWhenPaused(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fp := &fakePruner{summary: PruneSummary{Pruned: 3}}
	r.SetQueueDeps(nil, nil, fp)

	r.SetPaused(true)
	summary, ran := r.TriggerPrune(context.Background())
	if ran {
		t.Error("expected TriggerPrune to return ran=false when paused")
	}
	if summary != (PruneSummary{}) {
		t.Errorf("expected empty PruneSummary when paused, got %+v", summary)
	}
	if fp.calls != 0 {
		t.Errorf("expected 0 Prune calls when paused, got %d", fp.calls)
	}

	// When resumed, TriggerPrune works normally
	r.SetPaused(false)
	summary, ran = r.TriggerPrune(context.Background())
	if !ran || summary.Pruned != 3 {
		t.Errorf("expected TriggerPrune to run after resume, got ran=%v, summary=%+v", ran, summary)
	}
	if fp.calls != 1 {
		t.Errorf("expected 1 Prune call, got %d", fp.calls)
	}
}

func TestReconfigureDetectorDropsEventsWhenPaused(t *testing.T) {
	fi := &fakeIngester{}
	dir := t.TempDir()
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)
	r.SetPaused(true)

	var hit int32
	r.SetOnCardIngested(func() {
		atomic.AddInt32(&hit, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.ReconfigureDetector(ctx, []string{dir})

	cardDir1 := filepath.Join(dir, "CARD1")
	if err := os.Mkdir(cardDir1, 0o755); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&hit) > 0 || len(fi.calls) > 0 {
		t.Errorf("expected no ingest while paused, got hit=%d, calls=%d", hit, len(fi.calls))
	}

	// Resume ingest
	r.SetPaused(false)

	cardDir2 := filepath.Join(dir, "CARD2")
	if err := os.Mkdir(cardDir2, 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		if atomic.LoadInt32(&hit) > 0 && len(fi.calls) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&hit) == 0 || len(fi.calls) == 0 {
		t.Errorf("expected ingest to resume after unpausing, got hit=%d, calls=%d", hit, len(fi.calls))
	}

	r.StopDetector()
}

func TestTriggerDrainPauseOnMeteredCase3(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 2}}
	r.SetQueueDeps(nil, fd, nil)

	// Case 3: PauseUploadOnMetered=false and isMetered=true -> no behavior change, drain proceeds
	r.SetPauseUploadOnMetered(false)
	r.SetIsMeteredFunc(func() (bool, error) { return true, nil })
	_, ran := r.TriggerDrain(context.Background())
	if !ran {
		t.Error("expected TriggerDrain to return ran=true when PauseUploadOnMetered is false")
	}
	if fd.calls != 1 {
		t.Errorf("expected 1 Drain call, got %d", fd.calls)
	}
}

func TestRunnerIngestProgressRecordedAndCleared(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{{SourcePath: "a.jpg"}}}}
	r := NewRunner(fi, nil, "")

	st := r.Status(UpdateStatus{})
	if st.IngestProgress != nil {
		t.Errorf("expected IngestProgress=nil when idle, got %+v", st.IngestProgress)
	}

	// Set progress and check busy status
	r.setBusy(true, "/media/CANON R5")
	ev := ingest.ProgressEvent{
		Path:       "/local/DSC_0042.ARW",
		Phase:      ingest.ProgressPhaseCopying,
		BytesDone:  2469606195,
		TotalBytes: 8697308774,
	}
	r.SetProgress(&ev)

	st = r.Status(UpdateStatus{})
	if st.IngestProgress == nil {
		t.Fatal("expected IngestProgress non-nil while busy with progress set")
	}
	if st.IngestProgress.Path != "/local/DSC_0042.ARW" || st.IngestProgress.BytesDone != 2469606195 {
		t.Errorf("unexpected IngestProgress: %+v", st.IngestProgress)
	}

	// Clearing busy clears progress
	r.setBusy(false, "")
	st = r.Status(UpdateStatus{})
	if st.IngestProgress != nil {
		t.Errorf("expected IngestProgress cleared after setBusy(false), got %+v", st.IngestProgress)
	}
}

func TestFormatTooltipIdle(t *testing.T) {
	st := Status{Busy: false}
	if got := FormatTooltip(st); got != "branchDAM agent" {
		t.Errorf("FormatTooltip(idle) = %q, want %q", got, "branchDAM agent")
	}
}

func TestFormatTooltipBusyWithoutProgress(t *testing.T) {
	st := Status{Busy: true, BusyCard: "/Volumes/CANON R5"}
	if got := FormatTooltip(st); got != "Ingesting CANON R5..." {
		t.Errorf("FormatTooltip(busy, no progress) = %q, want %q", got, "Ingesting CANON R5...")
	}
}

func TestFormatTooltipBusyWithProgress(t *testing.T) {
	busySince := time.Now().Add(-5 * time.Second) // 2.3 GB in 5s ~ 460 MB/s
	ev := ingest.ProgressEvent{
		Path:       "/local/DSC_0042.ARW",
		Phase:      ingest.ProgressPhaseCopying,
		BytesDone:  2469606195, // 2.3 GB
		TotalBytes: 8697308774, // 8.1 GB
	}
	st := Status{
		Busy:           true,
		BusyCard:       "/Volumes/CANON R5",
		BusySince:      busySince,
		IngestProgress: &ev,
	}

	got := FormatTooltip(st)
	// Must contain card, filename, bytes, pct, and speed
	if !strings.HasPrefix(got, "Ingesting CANON R5: DSC_0042.ARW — 2.3 GB / 8.1 GB (28%") {
		t.Errorf("FormatTooltip got %q, want it to start with %q", got, "Ingesting CANON R5: DSC_0042.ARW — 2.3 GB / 8.1 GB (28%")
	}
	if !strings.Contains(got, "MB/s") {
		t.Errorf("FormatTooltip got %q, want it to contain speed MB/s", got)
	}
}

func TestFormatTooltipBusyWithProgressAndOfflineQueue(t *testing.T) {
	busySince := time.Now().Add(-5 * time.Second)
	ev := ingest.ProgressEvent{
		Path:       "/local/DSC_0042.ARW",
		Phase:      ingest.ProgressPhaseCopying,
		BytesDone:  2469606195,
		TotalBytes: 8697308774,
	}
	st := Status{
		Busy:           true,
		BusyCard:       "/Volumes/CANON R5",
		BusySince:      busySince,
		IngestProgress: &ev,
		QueueStatus: QueueStatus{
			Configured: true,
			Counts:     QueueCounts{AwaitingUpload: 1},
		},
	}

	if got := FormatTooltip(st); !strings.Contains(got, "(1 file queued offline)") {
		t.Errorf("FormatTooltip = %q, want queued-offline suffix", got)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{450 * 1024 * 1024, "450.0 MB"},
		{2469606195, "2.3 GB"},
		{8697308774, "8.1 GB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.bytes); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestFormatSpeed(t *testing.T) {
	cases := []struct {
		rate float64
		want string
	}{
		{500, "500 B/s"},
		{1500, "1 KB/s"},
		{450 * 1024 * 1024, "450 MB/s"},
		{2.5 * 1024 * 1024 * 1024, "2.5 GB/s"},
	}
	for _, tc := range cases {
		if got := formatSpeed(tc.rate); got != tc.want {
			t.Errorf("formatSpeed(%v) = %q, want %q", tc.rate, got, tc.want)
		}
	}
}

func TestFormatETA(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "~30s"},
		{14 * time.Minute, "~14 min"},
		{2 * time.Hour, "~2 hr"},
	}
	for _, tc := range cases {
		if got := formatETA(tc.d); got != tc.want {
			t.Errorf("formatETA(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestPauseDoesNotInterruptInProgressIngest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")

	done := make(chan IngestSummary, 1)
	go func() {
		done <- r.TriggerIngest(context.Background(), "/media/card")
	}()
	<-started

	// Set paused while ingest is in-flight
	r.SetPaused(true)

	// Ingest should still be marked busy
	if _, _, busy := r.Busy(); !busy {
		t.Error("expected in-flight ingest to remain busy")
	}

	// Release the in-flight ingest
	close(release)
	summary := <-done

	if summary.CardPath != "/media/card" {
		t.Errorf("got summary.CardPath = %q, want /media/card", summary.CardPath)
	}
	st := r.Status(UpdateStatus{})
	if st.LastIngest == nil {
		t.Error("expected LastIngest to be recorded for the in-progress ingest")
	}
	if !st.Paused {
		t.Error("expected Status().Paused to be true")
	}
}

// TestStatusSurfacesLastHandshakeAt pins the F-13 follow-up: a drain
// pass that stamps LastHandshakeAt must propagate that timestamp
// through to Status().LastHandshakeAt so the status page can render
// a freshness signal (issue #109 / PR #123 follow-up; the original PR
// shipped HandshakeOK as a bool, which collapses "5 minutes ago" and
// "3 weeks ago" into the same true).
func TestStatusSurfacesLastHandshakeAt(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
	r.SetQueueDeps(nil, fd, nil)

	r.TriggerDrain(context.Background())

	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.Equal(when) {
		t.Errorf("Status().LastHandshakeAt = %v, want %v", st.LastHandshakeAt, when)
	}
}

// TestStatusLastHandshakeAtZeroWhenNoDrains pins the zero-value
// sentinel: a never-drained install must surface
// LastHandshakeAt == time.Time{} so the template's
// {{ if not .Status.LastHandshakeAt.IsZero }} guard suppresses the
// "last handshake" line.
func TestStatusLastHandshakeAtZeroWhenNoDrains(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.IsZero() {
		t.Errorf("Status().LastHandshakeAt = %v, want zero time.Time{}", st.LastHandshakeAt)
	}
}

// TestStatusBusySinceResetsAfterIngest pins the B-17 follow-up:
// setBusy(false, ...) must reset r.busySince to time.Time{} so
// Status().BusySince does not return a stale timestamp from the
// last completed ingest. Before this fix, the status page's
// "Running… since HH:MM:SS" indicator kept showing the timestamp
// of the last ingest long after the tray went idle.
func TestStatusBusySinceResetsAfterIngest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fi := &blockingIngester{started: started, release: release}
	r := NewRunner(fi, nil, "")

	go r.TriggerIngest(context.Background(), "/media/a")
	<-started

	st := r.Status(UpdateStatus{})
	if !st.Busy {
		t.Fatalf("after ingest start: Busy=%v (should be true)", st.Busy)
	}
	if st.BusySince.IsZero() {
		t.Error("after ingest start: BusySince should be non-zero")
	}

	close(release)

	// Wait for the goroutine's post-IngestCard defer to run setBusy(false).
	// Polling with a deadline is race-safe and avoids depending on
	// blockingIngester's internal sync, which only signals start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, since, busy := r.Busy(); !busy {
			if !since.IsZero() {
				t.Errorf("after ingest completion: BusySince = %v, want time.Time{} (zero)", since)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	_, since, busy := r.Busy()
	t.Errorf("timed out waiting for ingest to complete: busy=%v since=%v", busy, since)
}

// TestStatusPreservesLastHandshakeAtAcrossFailedPass is the regression
// guard for the TriggerDrain carry-forward logic: when a drain pass
// with a failed handshake follows one that succeeded, the previous
// successful LastHandshakeAt must be preserved on the Status surface
// (issue #109 / audit F-13 follow-up; the "5s blip must not erase
// 'successful 4h ago'" invariant that the carry-forward logic
// exists to defend). The test pins two properties:
//
//  1. A single failure after a success preserves the prior stamp.
//  2. CONSECUTIVE failures after a success also preserve the prior
//     stamp (Hermes review on PR #148: keying the carry-forward off
//     r.lastDrain.HandshakeOK only survives ONE failure, because
//     after the carry-forward the prior summary's HandshakeOK is
//     false, so a second failure would drop the stamp -- a 10s+
//     outage at a 5s drain cadence would otherwise wipe the
//     freshness signal the field exists to defend).
//  3. An initial failure (no prior successful stamp) stays at the
//     zero sentinel so the template's "never completed a successful
//     handshake" line stays correct.
func TestStatusPreservesLastHandshakeAtAcrossFailedPass(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	t.Run("single_failure_after_success_preserves_stamp", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")

		fdSuccess := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
		r.SetQueueDeps(nil, fdSuccess, nil)
		r.TriggerDrain(context.Background())

		st := r.Status(UpdateStatus{})
		if !st.LastHandshakeAt.Equal(when) {
			t.Fatalf("after successful pass: LastHandshakeAt = %v, want %v", st.LastHandshakeAt, when)
		}

		fdFail := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
		r.SetQueueDeps(nil, fdFail, nil)
		r.TriggerDrain(context.Background())

		st = r.Status(UpdateStatus{})
		if !st.LastHandshakeAt.Equal(when) {
			t.Errorf("after single failed pass: LastHandshakeAt = %v, want %v (carry-forward of prior success)",
				st.LastHandshakeAt, when)
		}
	})

	t.Run("consecutive_failures_after_success_preserve_stamp", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")

		fdSuccess := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
		r.SetQueueDeps(nil, fdSuccess, nil)
		r.TriggerDrain(context.Background())

		fdFail := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
		r.SetQueueDeps(nil, fdFail, nil)
		r.TriggerDrain(context.Background())
		r.TriggerDrain(context.Background())
		r.TriggerDrain(context.Background())

		st := r.Status(UpdateStatus{})
		if !st.LastHandshakeAt.Equal(when) {
			t.Errorf("after three consecutive failed passes: LastHandshakeAt = %v, want %v (carry-forward must chain across multiple failures)",
				st.LastHandshakeAt, when)
		}
	})

	t.Run("initial_failure_stays_at_zero", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")

		fdFail := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
		r.SetQueueDeps(nil, fdFail, nil)
		r.TriggerDrain(context.Background())

		st := r.Status(UpdateStatus{})
		if !st.LastHandshakeAt.IsZero() {
			t.Errorf("after initial failed pass with no prior success: LastHandshakeAt = %v, want time.Time{} (never completed a successful handshake)",
				st.LastHandshakeAt)
		}
	})
}

// TestRunnerSeedLastHandshakeAtReflectsInStatus pins the cross-restart
// signal: a SeedLastHandshakeAt call on a fresh Runner must surface
// through to Status().LastHandshakeAt so a tray that loaded a
// persisted runtime.json at startup can render "last handshake: <since>
// ago" before the first in-session drain pass runs. This is the
// Status() half of issue #149's AC: the Load happens in runTrayCmd,
// this test pins the half that the tray internal itself owns.
func TestRunnerSeedLastHandshakeAtReflectsInStatus(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewRunner(&fakeIngester{}, nil, "")

	r.SeedLastHandshakeAt(when)

	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.Equal(when) {
		t.Errorf("Status().LastHandshakeAt = %v, want %v", st.LastHandshakeAt, when)
	}
	// Seed must also set HasDrained: the "have we ever heard from the
	// server?" signal the status page renders alongside the freshness
	// line should reflect a successful prior handshake, not "no drain
	// run yet."
	if !st.HasDrained {
		t.Error("expected Status().HasDrained=true after SeedLastHandshakeAt")
	}
	// Seed must NOT set HandshakeOK: that signal is the
	// current-session "last drain: handshake OK" line, and
	// no drain has run *in this session* yet. The status
	// page renders the prior-session stamp via
	// LastHandshakeAt separately from HandshakeOK. A
	// HandshakeOK=true from a seed would briefly say
	// "handshake OK" when the very first post-restart
	// drain hasn't completed -- misleading during the
	// 0-5s window before the drain timer's first tick.
	if st.HandshakeOK {
		t.Error("expected Status().HandshakeOK=false after SeedLastHandshakeAt (the current-session handshake-OK signal must come from a real drain, not a persisted seed)")
	}
}

// TestRunnerSeedLastHandshakeAtDoesNotConflateSeedWithDrained stamps
// the load-bearing case behind TestRunnerSeedLastHandshakeAtReflectsInStatus's
// HandshakeOK=false assertion: after a seed, a subsequent
// *successful* drain must reset HandshakeOK to true (a real
// in-session successful handshake now exists), and a subsequent
// *failed* drain must keep HandshakeOK false (a real failed
// handshake is the honest current-session signal). The test pins
// both transitions, since each is a separate code path on the
// happy/failed drain branches of TriggerDrain.
func TestRunnerSeedLastHandshakeAtDoesNotConflateSeedWithDrained(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	t.Run("failed_drain_after_seed_keeps_handshake_ok_false", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")
		r.SeedLastHandshakeAt(when)
		fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
		r.SetQueueDeps(nil, fd, nil)
		r.TriggerDrain(context.Background())

		st := r.Status(UpdateStatus{})
		if st.HandshakeOK {
			t.Error("after failed drain following a seed: HandshakeOK=true, want false (the failed drain's HandshakeOK is the current-session truth)")
		}
		if !st.LastHandshakeAt.Equal(when) {
			t.Errorf("after failed drain following a seed: LastHandshakeAt = %v, want %v (carry-forward preserves the seeded stamp)", st.LastHandshakeAt, when)
		}
	})

	t.Run("successful_drain_after_seed_sets_handshake_ok_true", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")
		r.SeedLastHandshakeAt(when)
		fresh := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
		fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: fresh}}
		r.SetQueueDeps(nil, fd, nil)
		r.TriggerDrain(context.Background())

		st := r.Status(UpdateStatus{})
		if !st.HandshakeOK {
			t.Error("after successful drain following a seed: HandshakeOK=false, want true (the real drain's HandshakeOK supersedes the seeded false)")
		}
		if !st.LastHandshakeAt.Equal(fresh) {
			t.Errorf("after successful drain following a seed: LastHandshakeAt = %v, want %v (the real drain's stamp supersedes the seed)", st.LastHandshakeAt, fresh)
		}
	})
}

// TestRunnerSeedLastHandshakeAtIgnoredWhenAlreadyDrained pins the
// "fresh signal beats persisted" invariant: if a real drain pass ran
// before the tray finished loading the runtime.json (e.g. the drain
// timer fired between NewRunner and SeedLastHandshakeAt), the real
// pass's stamp must win. Without this guard, a slower-loaded persisted
// stamp could overwrite a fresher in-memory one and the status page
// would briefly regress to an older "successful Nh ago" line.
func TestRunnerSeedLastHandshakeAtIgnoredWhenAlreadyDrained(t *testing.T) {
	fresh := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	persisted := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: fresh}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(context.Background())

	r.SeedLastHandshakeAt(persisted)

	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.Equal(fresh) {
		t.Errorf("Status().LastHandshakeAt = %v, want %v (real drain must beat persisted seed)", st.LastHandshakeAt, fresh)
	}
}

// TestRunnerSeedLastHandshakeAtWinsOverFailedDrain pins the guard
// fix for the reviewer's Critical #1: the seed must win when the
// in-memory state has a drain pass that *failed* (lastDrain non-nil
// with a zero LastHandshakeAt). A 5s drain cadence on an offline
// server has a high probability of hitting this race on a fresh
// install: NewRunner, then SetQueueDeps + go startPeriodic, then
// the wiring code that calls Seed. Without this guard, the seed
// is silently dropped when a single 5s blip precedes the load, and
// the cross-restart signal this whole PR exists to defend is lost
// for the entire session.
//
// The guard is the same expression as the in-memory carry-forward
// (Key Invariant 11: "r.lastDrain != nil &&
// !r.lastDrain.LastHandshakeAt.IsZero()"), so the seed wins
// whenever the in-memory state does not already have a successful
// stamp -- the same condition that the carry-forward would
// preserve a fresh pass's stamp into.
func TestRunnerSeedLastHandshakeAtWinsOverFailedDrain(t *testing.T) {
	persisted := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	r := NewRunner(&fakeIngester{}, nil, "")
	// Failed drain pass: leaves r.lastDrain non-nil with
	// LastHandshakeAt zero.
	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(context.Background())

	r.SeedLastHandshakeAt(persisted)

	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.Equal(persisted) {
		t.Errorf("after failed drain then seed: LastHandshakeAt = %v, want %v (seed must win when the in-memory state has no successful stamp yet)", st.LastHandshakeAt, persisted)
	}
}

// TestRunnerSeedLastHandshakeAtIgnoresZeroValue pins the "never
// completed a successful handshake" sentinel: a zero time.Time passed
// to SeedLastHandshakeAt must not pre-populate lastDrain, so a never-
// connected install never accidentally surfaces a spurious "successful
// 0001-01-01" line via the status page's
// {{ if not .Status.LastHandshakeAt.IsZero }} template guard.
func TestRunnerSeedLastHandshakeAtIgnoresZeroValue(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")

	r.SeedLastHandshakeAt(time.Time{})

	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.IsZero() {
		t.Errorf("Status().LastHandshakeAt = %v, want zero time.Time{} (zero seed must be a no-op)", st.LastHandshakeAt)
	}
	if st.HasDrained {
		t.Error("expected Status().HasDrained=false after zero-value SeedLastHandshakeAt")
	}
}

// TestTriggerDrainSuccessfulPassInvokesOnSuccessfulHandshake pins the
// happy path for issue #149's "write-back on every successful
// handshake" contract: when HandshakeOK is true, the Runner's
// registered callback fires with summary.LastHandshakeAt, which
// runTrayCmd wires to runtime.Save.
func TestTriggerDrainSuccessfulPassInvokesOnSuccessfulHandshake(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewRunner(&fakeIngester{}, nil, "")

	var got time.Time
	var calls int
	r.SetOnSuccessfulHandshake(func(t time.Time) error {
		got = t
		calls++
		return nil
	})

	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(context.Background())

	if calls != 1 {
		t.Fatalf("onSuccessfulHandshake calls = %d, want 1", calls)
	}
	if !got.Equal(when) {
		t.Errorf("callback t = %v, want %v", got, when)
	}
}

// TestTriggerDrainFailedPassDoesNotInvokeOnSuccessfulHandshake pins
// the "successful only" cadence: a failed handshake must NOT write
// runtime.json. Writing on failure would defeat the in-memory
// carry-forward (PR #148) by re-stamping a stale zero back to disk.
// A failed pass leaves the prior successful stamp in memory AND on
// disk unchanged.
func TestTriggerDrainFailedPassDoesNotInvokeOnSuccessfulHandshake(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")

	var calls int
	r.SetOnSuccessfulHandshake(func(time.Time) error { calls++; return nil })

	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 0, HandshakeOK: false}}
	r.SetQueueDeps(nil, fd, nil)
	r.TriggerDrain(context.Background())

	if calls != 0 {
		t.Errorf("onSuccessfulHandshake calls = %d, want 0 (failed pass must not write runtime state)", calls)
	}
}

// TestTriggerDrainWriteFailureDoesNotBreakDrain pins the "failing
// WriteFile must not block the drain" contract from issue #149's
// acceptance criteria. The callback is allowed to return an error
// (e.g. disk full, permission revocation) but the drain pass itself
// must still complete and surface HandshakeOK=true.
func TestTriggerDrainWriteFailureDoesNotBreakDrain(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewRunner(&fakeIngester{}, nil, "")

	r.SetOnSuccessfulHandshake(func(time.Time) error {
		return errors.New("disk full")
	})

	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
	r.SetQueueDeps(nil, fd, nil)
	summary, ran := r.TriggerDrain(context.Background())

	if !ran {
		t.Fatal("expected TriggerDrain to return ran=true")
	}
	if !summary.HandshakeOK {
		t.Error("expected HandshakeOK=true even when the save callback returns an error")
	}
	if !summary.LastHandshakeAt.Equal(when) {
		t.Errorf("summary.LastHandshakeAt = %v, want %v", summary.LastHandshakeAt, when)
	}
}

// TestTriggerDrainCallbackPanicDoesNotBreakDrain is the panic-half
// counterpart of TestTriggerDrainWriteFailureDoesNotBreakDrain: a
// misbehaving callback that panics must not corrupt the drain
// pass's state. The recover is in production code, the test pins
// it from the outside.
func TestTriggerDrainCallbackPanicDoesNotBreakDrain(t *testing.T) {
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	r := NewRunner(&fakeIngester{}, nil, "")

	r.SetOnSuccessfulHandshake(func(time.Time) error {
		panic("test-induced panic in save callback")
	})

	fd := &fakeDrainer{summary: DrainSummary{NodeCreatedSent: 1, HandshakeOK: true, LastHandshakeAt: when}}
	r.SetQueueDeps(nil, fd, nil)
	summary, ran := r.TriggerDrain(context.Background())

	if !ran {
		t.Fatal("expected TriggerDrain to return ran=true even after a panicking callback")
	}
	if !summary.HandshakeOK {
		t.Error("expected HandshakeOK=true after a panicking callback (drain must not be corrupted)")
	}
}

// TestCrossRestartLastHandshakeAtSurvivesViaRuntimeFile is the
// end-to-end test the issue specifically calls for: write a runtime
// state file (simulating a prior tray session's last successful
// drain), construct a fresh Runner, seed from the file, and assert
// Status().LastHandshakeAt reflects the persisted timestamp before
// any drain pass has run. This is the "Add a test that exercises the
// cross-restart path" AC.
func TestCrossRestartLastHandshakeAtSurvivesViaRuntimeFile(t *testing.T) {
	// Prior session's last successful handshake -- 4 hours before
	// "now" to make the human-readable freshness check obvious.
	prior := time.Now().Add(-4 * time.Hour).UTC().Truncate(time.Second)

	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.json")
	if err := osWriteJSON(rtPath, prior); err != nil {
		t.Fatal(err)
	}

	// Fresh session: new Runner, load the runtime state from disk.
	r := NewRunner(&fakeIngester{}, nil, "")

	// This is the wiring runTrayCmd does between NewRunner and the
	// drain timer start; the test is pinning exactly that sequence.
	loaded, err := loadRuntimeStateForTest(rtPath)
	if err != nil {
		t.Fatal(err)
	}
	r.SeedLastHandshakeAt(loaded)

	// No drain has run yet (the timer hasn't fired, the callback
	// hasn't been invoked). The persisted stamp must already be
	// visible on the status page, but HandshakeOK must remain
	// false: that signal is the current-session "last drain:
	// handshake OK" line, and a pre-restart successful handshake
	// is not the same as a post-restart one. See
	// TestRunnerSeedLastHandshakeAtReflectsInStatus for the
	// load-bearing assertion and the longer rationale.
	st := r.Status(UpdateStatus{})
	if !st.LastHandshakeAt.Equal(prior) {
		t.Errorf("after cross-restart seed: Status().LastHandshakeAt = %v, want %v (persisted)", st.LastHandshakeAt, prior)
	}
	if !st.HasDrained {
		t.Error("expected HasDrained=true after cross-restart seed")
	}
	if st.HandshakeOK {
		t.Error("expected HandshakeOK=false after cross-restart seed (current-session signal must come from a real drain, not a persisted stamp)")
	}
}

func TestAutoEjectOnSuccessfulIngest(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/media/CANON R5/DCIM/100CANON/IMG_0001.JPG"},
	}}}
	r := NewRunner(fi, []string{"/media"}, "/scratch")
	r.SetAutoEject(true)

	var ejectedPath string
	r.SetEjectFunc(func(mountPath string) error {
		ejectedPath = mountPath
		return nil
	})

	var notifTitle, notifMsg string
	r.SetNotifier(func(title, message string) {
		notifTitle = title
		notifMsg = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/CANON R5")
	if !summary.OK() {
		t.Fatalf("expected summary.OK()=true, got %+v", summary)
	}

	if ejectedPath != "/media/CANON R5" {
		t.Errorf("expected ejectedPath = %q, got %q", "/media/CANON R5", ejectedPath)
	}
	if notifTitle != "branchDAM Agent" {
		t.Errorf("expected notifTitle = %q, got %q", "branchDAM Agent", notifTitle)
	}
	if notifMsg != "CANON R5 ejected — safe to remove" {
		t.Errorf("expected notifMsg = %q, got %q", "CANON R5 ejected — safe to remove", notifMsg)
	}
}

func TestAutoEjectFailureNotification(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/media/SONY/DCIM/100MSDCF/DSC0001.ARW"},
	}}}
	r := NewRunner(fi, []string{"/media"}, "/scratch")
	r.SetAutoEject(true)

	r.SetEjectFunc(func(mountPath string) error {
		return errors.New("device busy")
	})

	var notifTitle, notifMsg string
	r.SetNotifier(func(title, message string) {
		notifTitle = title
		notifMsg = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/SONY")
	if !summary.OK() {
		t.Fatalf("expected summary.OK()=true, got %+v", summary)
	}

	if notifTitle != "branchDAM Agent" {
		t.Errorf("expected notifTitle = %q, got %q", "branchDAM Agent", notifTitle)
	}
	if notifMsg != "Eject failed — please eject manually" {
		t.Errorf("expected notifMsg = %q, got %q", "Eject failed — please eject manually", notifMsg)
	}
}

func TestAutoEjectSkippedOnIngestErrors(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/media/CARD/IMG_0001.JPG", Err: errors.New("hash mismatch")},
	}}}
	r := NewRunner(fi, []string{"/media"}, "/scratch")
	r.SetAutoEject(true)

	ejected := false
	r.SetEjectFunc(func(mountPath string) error {
		ejected = true
		return nil
	})

	var notifMsg string
	r.SetNotifier(func(title, message string) {
		notifMsg = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/CARD")
	if summary.OK() {
		t.Fatal("expected summary.OK()=false on ingest error")
	}

	if ejected {
		t.Error("expected card NOT to be ejected when ingest has errors")
	}
	if notifMsg != "" {
		t.Errorf("expected no notification on failed ingest, got %q", notifMsg)
	}
}

func TestAutoEjectDisabledByDefault(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/media/CARD/IMG_0001.JPG"},
	}}}
	r := NewRunner(fi, []string{"/media"}, "/scratch")
	if r.AutoEject() {
		t.Error("expected AutoEject()=false by default")
	}

	ejected := false
	r.SetEjectFunc(func(mountPath string) error {
		ejected = true
		return nil
	})

	var notifMsg string
	r.SetNotifier(func(title, message string) {
		notifMsg = message
	})

	summary := r.TriggerIngest(context.Background(), "/media/CARD")
	if !summary.OK() {
		t.Fatalf("expected summary.OK()=true, got %+v", summary)
	}

	if ejected {
		t.Error("expected card NOT to be ejected when autoEject is false")
	}
	if notifMsg != "1 photo imported from CARD" {
		t.Errorf("expected standard import notification, got %q", notifMsg)
	}
}
