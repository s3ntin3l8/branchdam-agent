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

// telemetryTimeout bounds one telemetry pass (a statfs plus one small
// POST).
const telemetryTimeout = 30 * time.Second

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

// runTelemetryPass is one telemetry tick: TriggerTelemetry itself skips
// while paused, while config is incomplete, with nothing configured, or
// with a pass already in flight. A failed send is logged, never retried --
// the next tick carries a fresher snapshot.
func runTelemetryPass(ctx context.Context, runner *tray.Runner) {
	if ran, err := runner.TriggerTelemetry(ctx); ran && err != nil {
		slog.Warn("telemetry send failed", "err", err)
	}
}
