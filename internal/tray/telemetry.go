package tray

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// TelemetrySender is the subset of *branchdam.Client the telemetry pass
// needs -- an interface, like Drainer/Pruner/ServerProbe, so tests can
// substitute a fake.
type TelemetrySender interface {
	SendTelemetry(ctx context.Context, in branchdam.AgentTelemetryInput) (*branchdam.AgentTelemetryOutput, error)
}

// telemetryDeps is what SetTelemetry wires and TriggerTelemetry reads,
// behind its own mutex so a settings reload can re-register a sender
// (new server.apiKey/baseUrl) without touching Runner.mu's critical
// sections.
type telemetryDeps struct {
	mu            sync.Mutex
	sender        TelemetrySender
	agentID       string
	clientVersion string
	// inFlight serializes passes: a slow POST never stacks a second one.
	inFlight sync.Mutex
}

// SetTelemetry registers (or, with a nil sender, clears) the
// POST /api/v1/agent/telemetry client and the identity it reports as.
// Re-called on every settings reload, same as SetServerProbe, so a rotated
// API key or base URL never leaves this posting through a stale client.
func (r *Runner) SetTelemetry(sender TelemetrySender, agentID, clientVersion string) {
	r.telemetry.mu.Lock()
	defer r.telemetry.mu.Unlock()
	r.telemetry.sender = sender
	r.telemetry.agentID = agentID
	r.telemetry.clientVersion = clientVersion
}

// TriggerTelemetry sends one scratch-storage/prune snapshot to the server:
// ingest.localEditRoot's volume capacity (total/free/used) and, once a
// prune pass has run this session, that pass's reclaimed bytes, duration
// and pruned-mirror count. ran=false covers "not configured" (no sender,
// or no localEditRoot), "ingest paused", "config incomplete", and "a pass
// already in flight" -- all quietly skipped, like TriggerDrain. A send
// failure is returned (and logged by the caller's loop) but never retried
// here: the next tick sends a fresher snapshot anyway.
//
// Gated on the session pause (AGENTS.md invariant 15's paused gate): an
// operator who paused the tray mid-shoot gets no background network calls
// from it. Called from its own timer goroutine (cmd/branchdam-agent's
// telemetry loop), never from the menu loop -- the POST may block. The
// disk usage comes from the same cached reading Status() uses
// (scratchUsageSnapshot): statfs on an unreachable share can hang with no
// context to bound it, and a hung call here would stall every later tick.
// Before the first background reading lands the snapshot reports zero
// capacity, which the server treats as "no verdict", not "disk full".
//
// The mirror/render-cache/proxy/prunable breakdown fields stay zero: the
// agent does not walk localEditRoot to size them, and a made-up number
// would be worse than an honest zero.
func (r *Runner) TriggerTelemetry(ctx context.Context) (ran bool, err error) {
	if r.paused.Load() || r.ConfigIncomplete() {
		return false, nil
	}
	if !r.telemetry.inFlight.TryLock() {
		return false, nil
	}
	defer r.telemetry.inFlight.Unlock()

	r.telemetry.mu.Lock()
	sender := r.telemetry.sender
	agentID := r.telemetry.agentID
	clientVersion := r.telemetry.clientVersion
	r.telemetry.mu.Unlock()

	r.mu.Lock()
	scratchDir := r.scratchDir
	lastPrune := r.lastPrune
	r.mu.Unlock()

	if sender == nil || scratchDir == "" {
		return false, nil
	}

	in := branchdam.AgentTelemetryInput{
		AgentID:        agentID,
		ClientVersion:  clientVersion,
		TimestampUnix:  time.Now().Unix(),
		ScratchStorage: branchdam.AgentScratchStorageDTO{MountPath: scratchDir},
	}
	if usage := r.scratchUsageSnapshot(scratchDir); usage.Err != "" {
		// Still send: the prune stats and the "agent is alive" timestamp
		// are useful on their own, and the dashboard shows zero capacity
		// rather than a stale reading.
		slog.Debug("telemetry: scratch disk usage unavailable", "path", scratchDir, "err", usage.Err)
	} else {
		in.ScratchStorage.TotalBytes = clampInt64(usage.TotalBytes)
		in.ScratchStorage.FreeBytes = clampInt64(usage.FreeBytes)
		if usage.TotalBytes >= usage.FreeBytes {
			in.ScratchStorage.UsedBytes = clampInt64(usage.TotalBytes - usage.FreeBytes)
		}
	}
	if lastPrune != nil {
		in.PruneStats = &branchdam.AgentPruneStatsDTO{
			LastPruneTimestampUnix: lastPrune.At.Unix(),
			LastReclaimedBytes:     lastPrune.FreedBytes,
			LastPruneDurationMs:    lastPrune.Duration.Milliseconds(),
			PrunedItemCounts:       map[string]int{"mirrors": lastPrune.Pruned},
		}
	}

	if _, err := sender.SendTelemetry(ctx, in); err != nil {
		return true, err
	}
	return true, nil
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
