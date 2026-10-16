package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// telemetryInterval is how often the tray reports scratch-storage and
// prune telemetry (POST /api/v1/agent/telemetry). The server keeps only
// the latest snapshot per agent and flags it stale after 24h, so a
// quarter-hour cadence keeps the Storage Health card current without
// adding meaningful traffic.
const telemetryInterval = 15 * time.Minute

// telemetryTimeout bounds one telemetry pass, which is one small POST:
// the pass reads the cached scratch-usage reading and never calls statfs
// itself.
const telemetryTimeout = 30 * time.Second

// startupUsageWait bounds how long the startup telemetry pass waits for
// the first scratch disk-usage reading. A few seconds covers any local or
// healthy network volume; statfs on an unreachable share can hang
// indefinitely, so the pass never waits longer than this.
const startupUsageWait = 5 * time.Second

// registerTelemetry wires (or clears) the Runner's telemetry sender,
// mirroring registerServerProbe: never against the dummy client
// resolveServerConfig builds while server.baseUrl/apiKey/agentId are
// incomplete, and re-called from configSettings.reload so a rotated key
// or base URL takes effect without a restart.
func registerTelemetry(runner *tray.Runner, client *branchdam.Client, cfg config.Config) {
	if !serverConfigured(cfg) {
		runner.SetTelemetry(nil, "", "")
		return
	}
	runner.SetTelemetry(client, cfg.AgentID, version)
}

// runStartupTelemetryPass is the pass the tray sends right after startup.
// The disk-usage cache is cold then, and the server keeps only the latest
// snapshot, so a pass sent at once would report zero capacity until the
// next tick 15 minutes later. It first waits up to usageWait for the
// background statfs reading (Runner.WaitScratchUsage), then sends; a
// statfs that doesn't answer in time leaves the old zero-capacity
// behaviour, never a stalled pass.
func runStartupTelemetryPass(ctx context.Context, runner *tray.Runner, usageWait time.Duration) {
	runner.WaitScratchUsage(ctx, usageWait)
	tctx, cancel := context.WithTimeout(ctx, telemetryTimeout)
	defer cancel()
	runTelemetryPass(tctx, runner)
}

// runTelemetryPass is one telemetry tick: TriggerTelemetry itself skips
// while paused, while config is incomplete, with nothing configured, or
// with a pass already in flight. A failed send is logged, never retried --
// the next tick carries a fresher snapshot.
func runTelemetryPass(ctx context.Context, runner *tray.Runner) {
	if ran, err := runner.TriggerTelemetry(ctx); ran && err != nil {
		slog.Warn("telemetry send failed", "err", err)
	}
}
