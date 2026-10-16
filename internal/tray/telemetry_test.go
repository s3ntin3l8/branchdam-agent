package tray

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

type fakeTelemetrySender struct {
	sent []branchdam.AgentTelemetryInput
	err  error
}

func (f *fakeTelemetrySender) SendTelemetry(_ context.Context, in branchdam.AgentTelemetryInput) (*branchdam.AgentTelemetryOutput, error) {
	f.sent = append(f.sent, in)
	if f.err != nil {
		return nil, f.err
	}
	return &branchdam.AgentTelemetryOutput{OK: true}, nil
}

// withDiskUsage stubs the disk-usage seam for the rest of t. The swap is
// atomic (setDiskUsageFunc), so a background refresh still running from
// this or an earlier test never races it.
func withDiskUsage(t *testing.T, fn func(string) (uint64, uint64, error)) {
	t.Helper()
	t.Cleanup(setDiskUsageFunc(fn))
}

// warmScratchUsage waits for the cached disk-usage reading telemetry
// reports (scratchUsageSnapshot refreshes it in the background).
func warmScratchUsage(t *testing.T, r *Runner, dir string) {
	t.Helper()
	if !waitFor(t, func() bool {
		u := r.scratchUsageSnapshot(dir)
		return u.TotalBytes != 0 || u.Err != ""
	}) {
		t.Fatal("scratch usage never refreshed")
	}
}

func TestTriggerTelemetrySendsScratchAndPruneStats(t *testing.T) {
	scratch := t.TempDir()
	withDiskUsage(t, func(string) (uint64, uint64, error) { return 300, 1000, nil })
	r := NewRunner(&fakeIngester{}, nil, scratch)
	sender := &fakeTelemetrySender{}
	r.SetTelemetry(sender, "agent-1", "1.2.3")
	warmScratchUsage(t, r, scratch)

	if ran, err := r.TriggerTelemetry(context.Background()); !ran || err != nil {
		t.Fatalf("TriggerTelemetry = (%v, %v), want (true, nil)", ran, err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d snapshots, want 1", len(sender.sent))
	}
	in := sender.sent[0]
	if in.AgentID != "agent-1" || in.ClientVersion != "1.2.3" || in.TimestampUnix == 0 {
		t.Errorf("identity = (%q, %q, %d)", in.AgentID, in.ClientVersion, in.TimestampUnix)
	}
	ss := in.ScratchStorage
	if ss.MountPath != scratch || ss.TotalBytes != 1000 || ss.FreeBytes != 300 || ss.UsedBytes != 700 {
		t.Errorf("scratchStorage = %+v", ss)
	}
	if in.PruneStats != nil {
		t.Errorf("pruneStats = %+v before any prune pass, want nil", in.PruneStats)
	}

	r.SetQueueDeps(nil, nil, &fakePruner{summary: PruneSummary{Evaluated: 5, Pruned: 3, FreedBytes: 4096}})
	if _, ran := r.TriggerPrune(context.Background()); !ran {
		t.Fatal("prune did not run")
	}
	if _, err := r.TriggerTelemetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	ps := sender.sent[1].PruneStats
	if ps == nil || ps.LastReclaimedBytes != 4096 || ps.LastPruneTimestampUnix == 0 || ps.PrunedItemCounts["mirrors"] != 3 {
		t.Errorf("pruneStats = %+v, want the last prune pass", ps)
	}
}

func TestTriggerTelemetryRespectsGates(t *testing.T) {
	withDiskUsage(t, func(string) (uint64, uint64, error) { return 1, 2, nil })

	t.Run("paused", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		sender := &fakeTelemetrySender{}
		r.SetTelemetry(sender, "a", "v")
		r.SetPaused(true)
		if ran, _ := r.TriggerTelemetry(context.Background()); ran || len(sender.sent) != 0 {
			t.Errorf("sent while paused (ran=%v, sent=%d)", ran, len(sender.sent))
		}
	})
	t.Run("config incomplete", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		sender := &fakeTelemetrySender{}
		r.SetTelemetry(sender, "a", "v")
		r.SetConfigIncomplete(true, []string{"server.apiKey"})
		if ran, _ := r.TriggerTelemetry(context.Background()); ran || len(sender.sent) != 0 {
			t.Errorf("sent with incomplete config (ran=%v, sent=%d)", ran, len(sender.sent))
		}
	})
	t.Run("no sender", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		if ran, err := r.TriggerTelemetry(context.Background()); ran || err != nil {
			t.Errorf("TriggerTelemetry with no sender = (%v, %v)", ran, err)
		}
	})
	t.Run("send error surfaced", func(t *testing.T) {
		dir := t.TempDir()
		r := NewRunner(&fakeIngester{}, nil, dir)
		boom := errors.New("server down")
		r.SetTelemetry(&fakeTelemetrySender{err: boom}, "a", "v")
		// Let the background refresh this Runner starts finish within
		// the subtest.
		warmScratchUsage(t, r, dir)
		if ran, err := r.TriggerTelemetry(context.Background()); !ran || !errors.Is(err, boom) {
			t.Errorf("TriggerTelemetry = (%v, %v), want (true, %v)", ran, err, boom)
		}
	})
}

// TestTriggerTelemetryDoesNotBlockOnHungStatfs: statfs on an unreachable
// share can hang with no context to bound it. Telemetry reads the cached
// reading instead, so a hung call never stalls the telemetry loop; the
// pass sends zero capacity, which the server treats as no verdict.
func TestTriggerTelemetryDoesNotBlockOnHungStatfs(t *testing.T) {
	release := make(chan struct{})
	// started closes once the background refresh has entered the stub;
	// waiting on it before returning confirms the hung call really was in
	// flight while TriggerTelemetry returned.
	started := make(chan struct{})
	var once sync.Once
	defer close(release)
	withDiskUsage(t, func(string) (uint64, uint64, error) {
		once.Do(func() { close(started) })
		<-release
		return 1, 2, nil
	})
	r := NewRunner(&fakeIngester{}, nil, t.TempDir())
	sender := &fakeTelemetrySender{}
	r.SetTelemetry(sender, "a", "v")

	done := make(chan struct{})
	go func() {
		_, _ = r.TriggerTelemetry(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerTelemetry blocked on a hung disk-usage call")
	}
	if len(sender.sent) != 1 || sender.sent[0].ScratchStorage.TotalBytes != 0 {
		t.Errorf("sent = %+v, want one snapshot with zero capacity", sender.sent)
	}
	<-started
}

type slowPruner struct{ delay time.Duration }

func (p slowPruner) Prune(context.Context) (PruneSummary, error) {
	time.Sleep(p.delay)
	return PruneSummary{Pruned: 1}, nil
}

// TestPruneDurationReachesTelemetryAndJSON pins TriggerPrune's Duration
// stamp: telemetry reports it in milliseconds, and PruneSummary's JSON
// carries durationMs, never raw nanoseconds.
func TestPruneDurationReachesTelemetryAndJSON(t *testing.T) {
	scratch := t.TempDir()
	withDiskUsage(t, func(string) (uint64, uint64, error) { return 1, 2, nil })
	r := NewRunner(&fakeIngester{}, nil, scratch)
	sender := &fakeTelemetrySender{}
	r.SetTelemetry(sender, "a", "v")
	r.SetQueueDeps(nil, nil, slowPruner{delay: 20 * time.Millisecond})
	warmScratchUsage(t, r, scratch)

	summary, ran := r.TriggerPrune(context.Background())
	if !ran {
		t.Fatal("prune did not run")
	}
	if _, err := r.TriggerTelemetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps := sender.sent[0].PruneStats; ps == nil || ps.LastPruneDurationMs < 20 {
		t.Errorf("pruneStats = %+v, want lastPruneDurationMs >= 20", ps)
	}

	b, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"durationMs":`) || strings.Contains(string(b), `"Duration"`) {
		t.Errorf("PruneSummary JSON = %s, want durationMs and no raw Duration", b)
	}
}

// TestWaitScratchUsage: the wait returns as soon as the first reading
// lands, is bounded when statfs hangs (it must never stall the startup
// telemetry pass), and returns at once with no localEditRoot.
func TestWaitScratchUsage(t *testing.T) {
	t.Run("reading lands", func(t *testing.T) {
		withDiskUsage(t, func(string) (uint64, uint64, error) { return 300, 1000, nil })
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		if !r.WaitScratchUsage(context.Background(), 5*time.Second) {
			t.Fatal("WaitScratchUsage = false, want the reading")
		}
		if u := r.scratchUsageSnapshot(r.scratchDir); u.TotalBytes != 1000 {
			t.Errorf("cached reading = %+v, want TotalBytes 1000", u)
		}
	})
	t.Run("hung statfs is bounded", func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		withDiskUsage(t, func(string) (uint64, uint64, error) {
			<-release
			return 1, 2, nil
		})
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		start := time.Now()
		if r.WaitScratchUsage(context.Background(), 100*time.Millisecond) {
			t.Fatal("WaitScratchUsage = true while statfs hangs")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("WaitScratchUsage took %v with a 100ms bound", elapsed)
		}
	})
	t.Run("no localEditRoot", func(t *testing.T) {
		r := NewRunner(&fakeIngester{}, nil, "")
		if r.WaitScratchUsage(context.Background(), time.Hour) {
			t.Error("WaitScratchUsage = true with no localEditRoot")
		}
	})
}
