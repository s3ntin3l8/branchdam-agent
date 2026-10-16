package tray

import (
	"context"
	"errors"
	"testing"

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

func withDiskUsage(t *testing.T, fn func(string) (uint64, uint64, error)) {
	t.Helper()
	orig := diskUsageFunc
	diskUsageFunc = fn
	t.Cleanup(func() { diskUsageFunc = orig })
}

func TestTriggerTelemetrySendsScratchAndPruneStats(t *testing.T) {
	scratch := t.TempDir()
	withDiskUsage(t, func(string) (uint64, uint64, error) { return 300, 1000, nil })
	r := NewRunner(&fakeIngester{}, nil, scratch)
	sender := &fakeTelemetrySender{}
	r.SetTelemetry(sender, "agent-1", "1.2.3")

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
		r := NewRunner(&fakeIngester{}, nil, t.TempDir())
		boom := errors.New("server down")
		r.SetTelemetry(&fakeTelemetrySender{err: boom}, "a", "v")
		if ran, err := r.TriggerTelemetry(context.Background()); !ran || !errors.Is(err, boom) {
			t.Errorf("TriggerTelemetry = (%v, %v), want (true, %v)", ran, err, boom)
		}
	})
}
