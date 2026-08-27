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

func TestStatusQueueStatusIsAlwaysTheStub(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	st := r.Status(UpdateStatus{Enabled: true, Checked: true, CurrentVersion: "1.0.0"})
	if st.QueueStatus != QueueStatusStub {
		t.Errorf("got %q, want the literal stub -- never a fabricated number before M2", st.QueueStatus)
	}
	if st.SelfUpdate.Note() != "up to date (1.0.0)" {
		t.Errorf("got %q", st.SelfUpdate.Note())
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
