package tray

import (
	"context"
	"errors"
	"strings"
	"testing"

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

	st := r.Status("disabled")
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
	st := r.Status("up to date")
	if st.QueueStatus != QueueStatusStub {
		t.Errorf("got %q, want the literal stub -- never a fabricated number before M2", st.QueueStatus)
	}
	if st.SelfUpdateNote != "up to date" {
		t.Errorf("got %q", st.SelfUpdateNote)
	}
}

func TestStatusScratchNote(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	if got := r.Status("").ScratchNote; got != "not configured" {
		t.Errorf("got %q, want 'not configured' for an empty ScratchDir", got)
	}

	r2 := NewRunner(&fakeIngester{}, nil, "/local/scratch")
	got := r2.Status("").ScratchNote
	if !strings.Contains(got, "/local/scratch") || !strings.Contains(got, "not yet implemented") {
		t.Errorf("got %q, want the configured path plus an explicit not-yet-implemented note (never a fabricated usage number)", got)
	}
}

func TestStatusWatchDirsIsACopy(t *testing.T) {
	dirs := []string{"/media/a"}
	r := NewRunner(&fakeIngester{}, dirs, "")
	st := r.Status("")
	st.WatchDirs[0] = "mutated"
	if r.WatchDirs[0] != "/media/a" {
		t.Error("Status() must return a copy of WatchDirs, not the live slice")
	}
}
