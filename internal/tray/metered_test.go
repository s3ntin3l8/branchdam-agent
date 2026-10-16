package tray

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// TestStatusDoesNotBlockOnMeteredProbe: Status() feeds the tray's menu
// loop, so a stalled metered probe must not stall it.
func TestStatusDoesNotBlockOnMeteredProbe(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetPauseUploadOnMetered(true)
	release := make(chan struct{})
	var calls int32
	r.SetIsMeteredFunc(func() (bool, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return true, nil
	})

	done := make(chan Status, 1)
	go func() { done <- r.Status(UpdateStatus{}) }()
	select {
	case st := <-done:
		if st.UploadPausedMetered {
			t.Error("UploadPausedMetered=true before any probe completed")
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Status() blocked on the metered probe")
	}
	// A second Status() while the probe is still in flight must not start
	// another one.
	if !waitFor(t, func() bool { return atomic.LoadInt32(&calls) == 1 }) {
		close(release)
		t.Fatal("background metered probe never started")
	}
	_ = r.Status(UpdateStatus{})
	time.Sleep(20 * time.Millisecond)
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("probe started %d times while one was in flight, want 1", n)
	}

	close(release)
	if !waitFor(t, func() bool { return r.Status(UpdateStatus{}).UploadPausedMetered }) {
		t.Error("UploadPausedMetered never became true after the probe reported metered")
	}
}

func TestStatusMeteredProbeErrorShownFailClosed(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetPauseUploadOnMetered(true)
	r.SetIsMeteredFunc(func() (bool, error) { return false, errors.New("probe failed") })
	_ = r.Status(UpdateStatus{})
	if !waitFor(t, func() bool { return r.Status(UpdateStatus{}).UploadPausedMetered }) {
		t.Error("a failed probe must show uploads as paused, matching the drain gate's fail-closed rule")
	}
}

func TestStatusMeteredNotProbedWhenPauseOnMeteredOff(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetIsMeteredFunc(func() (bool, error) {
		t.Error("metered probe ran with pauseUploadOnMetered off")
		return true, nil
	})
	if r.Status(UpdateStatus{}).UploadPausedMetered {
		t.Error("UploadPausedMetered=true with pauseUploadOnMetered off")
	}
	time.Sleep(20 * time.Millisecond)
}

// TestTriggerDrainMeteredProbeErrorFailsClosed pins invariant 15 at the
// gate itself: the drain gate never reads the display cache, and a probe
// error skips the pass.
func TestTriggerDrainMeteredProbeErrorFailsClosed(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	fd := &fakeDrainer{}
	r.SetQueueDeps(nil, fd, nil)
	r.SetPauseUploadOnMetered(true)
	r.SetIsMeteredFunc(func() (bool, error) { return false, errors.New("probe failed") })
	if _, ran := r.TriggerDrain(context.Background()); ran || fd.calls != 0 {
		t.Errorf("TriggerDrain ran=%v calls=%d on a failed metered probe, want skipped", ran, fd.calls)
	}
}

// TestTrayTooltipReflectsLiveState is the regression test for the tooltip
// being overwritten with a static string on every refresh.
func TestTrayTooltipReflectsLiveState(t *testing.T) {
	cases := []struct {
		name string
		st   Status
		want string
	}{
		{"idle", Status{}, "branchDAM agent"},
		{"not configured", Status{ConfigIncomplete: true, Paused: true}, "not configured"},
		{"paused", Status{Paused: true, UploadPausedMetered: true}, "ingest paused"},
		{"ingesting", Status{Busy: true, BusyCard: "/Volumes/EOS_DIGITAL"}, "Ingesting EOS_DIGITAL"},
		{"progress", Status{Busy: true, BusyCard: "/Volumes/CARD", IngestProgress: &ingest.ProgressEvent{Path: "/x/IMG_1.CR3", BytesDone: 1, TotalBytes: 2}}, "IMG_1.CR3"},
		{"queued offline", Status{QueueStatus: QueueStatus{Counts: QueueCounts{AwaitingUpload: 3}}}, "3 files queued offline"},
		{"metered", Status{UploadPausedMetered: true}, "upload paused (metered connection)"},
		{"metered while ingesting", Status{Busy: true, BusyCard: "/Volumes/CARD", UploadPausedMetered: true}, "Ingesting CARD"},
	}
	for _, tc := range cases {
		got := TrayTooltip(tc.st)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: TrayTooltip = %q, want it to contain %q", tc.name, got, tc.want)
		}
	}
	if got := TrayTooltip(Status{Busy: true, BusyCard: "/Volumes/CARD", UploadPausedMetered: true}); !strings.Contains(got, "metered") {
		t.Errorf("metered note missing while ingesting: %q", got)
	}
}

// TestStalledMeteredProbeIsAbandoned: a probe that never returns must not
// freeze the display at its last answer. Past meteredProbeStall the cache
// reads as metered (fail-closed), a new probe starts, and the stalled
// probe's late answer is discarded.
func TestStalledMeteredProbeIsAbandoned(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetPauseUploadOnMetered(true)
	stuck := make(chan struct{})
	var calls int32
	r.SetIsMeteredFunc(func() (bool, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			<-stuck
			return false, nil
		}
		return true, nil
	})

	_ = r.Status(UpdateStatus{})
	if !waitFor(t, func() bool { return atomic.LoadInt32(&calls) == 1 }) {
		t.Fatal("first probe never started")
	}
	// Age the in-flight probe past the stall bound.
	r.meteredCache.mu.Lock()
	r.meteredCache.started = time.Now().Add(-meteredProbeStall)
	r.meteredCache.mu.Unlock()

	if !r.Status(UpdateStatus{}).UploadPausedMetered {
		t.Error("stalled probe: UploadPausedMetered = false, want true (fail-closed)")
	}
	if !waitFor(t, func() bool { return atomic.LoadInt32(&calls) == 2 }) {
		t.Fatal("no fresh probe started after the stall")
	}
	if !waitFor(t, func() bool {
		r.meteredCache.mu.Lock()
		defer r.meteredCache.mu.Unlock()
		return r.meteredCache.probed && r.meteredCache.metered
	}) {
		t.Fatal("fresh probe's answer was never stored")
	}

	// The stalled probe finally answers "not metered": it must not
	// overwrite the newer answer.
	close(stuck)
	time.Sleep(50 * time.Millisecond)
	if !r.Status(UpdateStatus{}).UploadPausedMetered {
		t.Error("stalled probe's late answer overwrote the newer one")
	}
}
