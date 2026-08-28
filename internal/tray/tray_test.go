package tray

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// fakeIngester substitutes internal/ingest.Engine for tests, matching the
// pattern internal/ingest.Engine's own nodeCreator interface and
// cmd/branchdam-agent/preflight.go's helloCaller use.
type fakeIngester struct {
	result ingest.CardResult
	err    error
	calls  []string
}

func (f *fakeIngester) IngestCard(_ context.Context, cardRoot string) (ingest.CardResult, error) {
	f.calls = append(f.calls, cardRoot)
	return f.result, f.err
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
func (fakeSettings) Reload() error                              { return nil }
func (fakeSettings) OpenConfigFile() error                      { return nil }
func (fakeSettings) RevealConfigFolder() error                  { return nil }

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
