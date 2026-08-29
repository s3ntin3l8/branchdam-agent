package main

import (
	"context"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/luminar"
	"github.com/s3ntin3l8/branchdam-agent/internal/nodeindex"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// IntegrationBuilder is the EXECUTION-side registry entry cmd/branchdam-agent
// owns -- the counterpart to internal/tray.Integrations()'s presentation
// registry, kept in a separate package specifically so internal/tray never
// imports internal/luminar (see tray.IntegrationSyncer's own doc comment).
// Adding lrcat (#47)/applephotos (#46) means appending one entry here plus
// one to tray.Integrations() -- TestRegistryCompleteness (in
// integrations_test.go) fails the build if the two ever fall out of
// bijection. A slice, not a map, since order should match
// tray.Integrations()'s own order.
type IntegrationBuilder struct {
	ID tray.IntegrationID
	// Ready reports whether cfg has everything this integration needs to
	// actually run -- an integration that is disabled, or enabled but
	// missing a required path, is simply absent from
	// buildIntegrationDeps' returned map (a nil syncer is Runner's honest
	// "not configured" signal), never an error. Deliberately NOT a
	// config.Validate() rule -- see
	// config.CatalogSyncConfig.Enabled's own doc comment for why a
	// cross-field completeness check there would deadlock the (later)
	// Settings menu.
	Ready func(cfg config.Config) bool
	// New builds the syncer once Ready reports true.
	New func(cfg config.Config, client *branchdam.Client) tray.IntegrationSyncer
	// Interval returns this integration's own sync interval given the
	// CURRENT config -- used by runTrayCmd's per-builder
	// startPeriodicVar goroutine so each integration schedules
	// independently and picks up a live config change (e.g. from a
	// later PR's Settings menu) without a restart. A value <= 0 means
	// "manual only" -- see config.CatalogSyncConfig.SyncIntervalMinutes's
	// own doc comment on why that's a real, deliberate mode, not an
	// error.
	Interval func(cfg config.Config) time.Duration
}

// integrationBuilders is the compile-time execution registry, in the same
// order as tray.Integrations().
var integrationBuilders = []IntegrationBuilder{
	{
		ID: tray.IntegrationLuminar,
		Ready: func(cfg config.Config) bool {
			l := cfg.Integrations.Luminar
			if !l.Enabled || l.CatalogPath == "" {
				return false
			}
			// A real (non-dry-run) sync needs the node index to resolve
			// either endpoint at all -- without it every candidate pair
			// is unconditionally skipped, which is a worse failure mode
			// than simply not registering the syncer in the first place.
			if !l.DryRun && cfg.Integrations.NodeIndexPath == "" {
				return false
			}
			return true
		},
		New: func(cfg config.Config, client *branchdam.Client) tray.IntegrationSyncer {
			l := cfg.Integrations.Luminar
			var edgeClient luminar.EdgeAttacher
			if !l.DryRun {
				edgeClient = client
			}
			return &luminarSyncer{
				client:        edgeClient,
				agentID:       cfg.AgentID,
				catalogPath:   l.CatalogPath,
				nodeIndexPath: cfg.Integrations.NodeIndexPath,
				dryRun:        l.DryRun,
				timeout:       time.Duration(l.TimeoutSecsOrDefault()) * time.Second,
			}
		},
		Interval: func(cfg config.Config) time.Duration {
			return time.Duration(cfg.Integrations.Luminar.SyncIntervalMinutesOrDefault()) * time.Minute
		},
	},
}

// buildIntegrationDeps constructs one tray.IntegrationSyncer per builder
// whose Ready reports true against cfg, keyed by ID. An integration that
// is disabled, or enabled but missing a required path, is simply absent
// from the returned map -- see IntegrationBuilder.Ready's own doc comment.
// Called from runTrayCmd at startup and from configSettings.reload() on
// every settings change, exactly mirroring queueDrainer/queuePruner's own
// rebuild-on-reload contract (see reload()'s doc comment): omitting the
// reload() call site would leave a syncer POSTing with a stale client
// after a server.apiKey rotation.
func buildIntegrationDeps(cfg config.Config, client *branchdam.Client) map[tray.IntegrationID]tray.IntegrationSyncer {
	deps := make(map[tray.IntegrationID]tray.IntegrationSyncer, len(integrationBuilders))
	for _, b := range integrationBuilders {
		if b.Ready(cfg) {
			deps[b.ID] = b.New(cfg, client)
		}
	}
	return deps
}

// luminarSyncer implements tray.IntegrationSyncer over internal/luminar --
// the concrete wiring cmd/branchdam-agent owns, matching queueDrainer's own
// relationship to tray.Drainer.
//
// It stores PATHS, not open handles: luminar.Open and nodeindex.Load both
// run inside Sync and are released before it returns (see catalog/close
// below) -- the operator may have the catalog open live in Luminar right
// now, and re-resolving the node index per pass means a regenerated index
// file is picked up without a tray restart. client is nil in dry-run mode,
// mirroring runLuminarSyncCmd's own `var client luminar.EdgeAttacher` treatment.
type luminarSyncer struct {
	client        luminar.EdgeAttacher
	agentID       string
	catalogPath   string
	nodeIndexPath string
	dryRun        bool
	timeout       time.Duration
}

func (s *luminarSyncer) Sync(ctx context.Context) (tray.SyncSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	catalog, err := luminar.Open(ctx, s.catalogPath)
	if err != nil {
		return tray.SyncSummary{DryRun: s.dryRun}, err
	}
	defer func() { _ = catalog.Close() }()

	// A dry run needs no node index at all -- every candidate simply
	// can't resolve, which the summary's Skipped count already conveys.
	// buildIntegrationDeps' Ready check requires a node index for a real
	// (non-dry-run) sync, but a dry run must stay usable even before an
	// operator has generated one.
	var index nodeindex.Resolver
	if s.nodeIndexPath != "" {
		idx, err := nodeindex.Load(s.nodeIndexPath)
		if err != nil {
			return tray.SyncSummary{DryRun: s.dryRun}, err
		}
		index = idx
	} else {
		index = emptyNodeIndex{}
	}

	syncer := &luminar.Syncer{
		Catalog: catalog,
		Index:   index,
		Client:  s.client,
		AgentID: s.agentID,
		DryRun:  s.dryRun,
	}

	stats, err := syncer.Sync(ctx)
	summary := tray.SyncSummary{
		DryRun:     s.dryRun,
		PairsFound: stats.PairsFound,
		Emitted:    stats.Emitted,
		Skipped:    stats.SourceUnresolved + stats.EditUnresolved,
		Errors:     stats.Errors,
	}
	return summary, err
}

// emptyNodeIndex is nodeindex.Resolver's zero-entry implementation, used
// when a dry run is requested with no nodeIndexPath configured yet --
// every path simply fails to resolve (ok=false), which PairDerivatives'
// caller already treats as an ordinary, logged skip, never an error.
type emptyNodeIndex struct{}

func (emptyNodeIndex) Resolve(_ string) (string, bool, error) { return "", false, nil }

// startPeriodicVar is startPeriodic (queueagent.go) with a DYNAMIC
// interval, re-read from interval() before every cycle, rather than
// captured once at ticker construction. The integration sync timer needs
// this and the drain/prune timers do not: offline.drainIntervalSecs and
// prune.intervalMinutes are documented config-file-only precisely because
// startPeriodic's ticker can't be changed after the fact, whereas
// integrations.luminar.syncIntervalMinutes IS meant to be editable from a
// later PR's Settings menu with no restart required.
//
// interval() <= 0 means "manual only, or not yet configured" -- the loop
// sleeps checkInterval and re-evaluates rather than exiting, so flipping
// an integration back on (or shortening its interval) from the menu takes
// effect within one checkInterval, not only after a restart.
func startPeriodicVar(ctx context.Context, checkInterval time.Duration, interval func() time.Duration, timeout time.Duration, fn func(context.Context)) {
	timer := time.NewTimer(checkInterval)
	defer timer.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			iv := interval()
			if iv > 0 && (last.IsZero() || time.Since(last) >= iv) {
				fnCtx, cancel := context.WithTimeout(ctx, timeout)
				fn(fnCtx)
				cancel()
				last = time.Now()
			}
			timer.Reset(checkInterval)
		}
	}
}

// integrationSyncCheckInterval is startPeriodicVar's own polling cadence
// for re-reading integrations.*.syncIntervalMinutes and re-evaluating
// whether any integration is due -- deliberately much finer than any real
// sync interval (minutes), so a menu-driven enable/interval change is
// picked up quickly without needing a restart.
const integrationSyncCheckInterval = 30 * time.Second

// integrationSyncTimeout bounds one timer-driven or menu-driven
// ("Sync now") integration sync pass. Deliberately its own constant, not
// periodicPassTimeout (2 minutes, sized for a queue drain/prune pass): a
// large Luminar catalog plus a per-edge POST loop can run considerably
// longer than a drain pass, and unlike drain/prune (5s/30m ticks), a sync
// runs at most once an hour by default -- there is no cost to a more
// generous ceiling.
const integrationSyncTimeout = 10 * time.Minute
