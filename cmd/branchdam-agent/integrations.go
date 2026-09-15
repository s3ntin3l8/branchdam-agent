package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/luminar"
	"github.com/s3ntin3l8/branchdam-agent/internal/nodeindex"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolve"
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
	// Title must match the corresponding tray.Integrations() entry's own
	// Title exactly -- TestRegistryCompleteness asserts this. Used by the
	// Settings/dialog wiring (settings.go) for dialog titles and error
	// messages; the menu itself (internal/tray) reads Title from its own
	// registry, never from here.
	Title string

	// Current extracts this integration's own CatalogSyncConfig out of
	// the whole Config -- the ONE place cfg.Integrations.Luminar (or a
	// future .Lrcat/.ApplePhotos) is read by name, so every other piece
	// of the Settings/dialog wiring (Snapshot, PromptAndSetIntegrationPath,
	// applyIntegration*Change) stays ID-generic instead of switching on ID.
	Current func(cfg config.Config) config.CatalogSyncConfig
	// Apply is Current's write-side counterpart -- Go has no generic way
	// to address cfg.Integrations.Luminar vs. a future cfg.Integrations.Lrcat
	// by ID string alone, so a settings change is applied via this closure
	// rather than reflection.
	Apply func(cfg *config.Config, c config.CatalogSyncConfig)

	// CatalogFilePatterns are the file-picker filter patterns for this
	// integration's catalog file (e.g. {"*.db", "*.catalog", "*"} for
	// Luminar) -- the trailing "*" is deliberate: the real on-disk
	// extension isn't documented to be stable across catalog-app
	// versions, and a picker that can't select an unexpected extension is
	// worse than an unfiltered one.
	CatalogFilePatterns []string
	// DatabaseURL marks a credential-bearing database connection string.
	// Its settings dialog is a hidden text entry with no argv-visible
	// default, rather than a filesystem picker.
	DatabaseURL bool

	// Ready reports whether cfg has everything this integration needs to
	// actually run -- an integration that is disabled, or enabled but
	// missing a required path, is simply absent from
	// buildIntegrationDeps' returned map (a nil syncer is Runner's honest
	// "not configured" signal), never an error. Deliberately NOT a
	// config.Validate() rule -- see
	// config.CatalogSyncConfig.Enabled's own doc comment for why a
	// cross-field completeness check there would deadlock the Settings
	// menu.
	Ready func(cfg config.Config) bool
	// New builds the syncer once Ready reports true.
	New func(cfg config.Config, client *branchdam.Client) tray.IntegrationSyncer
	// Interval returns this integration's own sync interval given the
	// CURRENT config -- used by runTrayCmd's per-builder
	// startPeriodicVar goroutine so each integration schedules
	// independently and picks up a live config change from the Settings
	// menu without a restart. A value <= 0 means "manual only" -- see
	// config.CatalogSyncConfig.SyncIntervalMinutes's own doc comment on
	// why that's a real, deliberate mode, not an error.
	Interval func(cfg config.Config) time.Duration

	// CurrentRewrites formats the integration's path rewrite rules as a
	// "from:to, from:to" string for display. Nil for integrations without
	// pathRewrites (Luminar).
	CurrentRewrites func(cfg config.Config) string
	// ApplyRewrites parses a "from:to, from:to" string and applies it to
	// the integration's config. Nil for integrations without pathRewrites.
	ApplyRewrites func(cfg *config.Config, v string) error
}

// ConfigKey builds the dotted config.yaml key for one of this
// integration's own leaves ("integrations.<id>.<leaf>") -- the one place
// that string is spelled on the execution side, mirroring
// internal/tray's own unexported integrationKey helper (which derives the
// identical string independently, since internal/tray cannot import this
// package's registry). Both must agree on the schema
// config.IntegrationsConfig defines.
func (b IntegrationBuilder) ConfigKey(leaf string) string {
	return "integrations." + string(b.ID) + "." + leaf
}

// builderFor looks up integrationBuilders by ID, mirroring
// tray.SettingsView.Integration/tray.Status.Integration's own by-ID-not-index
// lookup convention.
func builderFor(id tray.IntegrationID) (IntegrationBuilder, bool) {
	for _, b := range integrationBuilders {
		if b.ID == id {
			return b, true
		}
	}
	return IntegrationBuilder{}, false
}

// integrationBuilders is the compile-time execution registry, in the same
// order as tray.Integrations().
var integrationBuilders = []IntegrationBuilder{
	{
		ID:    tray.IntegrationLuminar,
		Title: "Luminar Neo",
		Current: func(cfg config.Config) config.CatalogSyncConfig {
			return cfg.Integrations.Luminar
		},
		Apply: func(cfg *config.Config, c config.CatalogSyncConfig) {
			cfg.Integrations.Luminar = c
		},
		CatalogFilePatterns: []string{"*.db", "*.catalog", "*"},
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
	{
		ID:    tray.IntegrationResolveDB,
		Title: "DaVinci Resolve",
		Current: func(cfg config.Config) config.CatalogSyncConfig {
			r := cfg.Integrations.ResolveDB
			return config.CatalogSyncConfig{
				Enabled:             r.Enabled,
				CatalogPath:         r.DatabaseURL,
				DryRun:              r.DryRun,
				SyncIntervalMinutes: r.SyncIntervalMinutes,
				TimeoutSecs:         r.TimeoutSecs,
			}
		},
		Apply: func(cfg *config.Config, c config.CatalogSyncConfig) {
			cfg.Integrations.ResolveDB.Enabled = c.Enabled
			cfg.Integrations.ResolveDB.DatabaseURL = c.CatalogPath
			cfg.Integrations.ResolveDB.DryRun = c.DryRun
			cfg.Integrations.ResolveDB.SyncIntervalMinutes = c.SyncIntervalMinutes
			cfg.Integrations.ResolveDB.TimeoutSecs = c.TimeoutSecs
		},
		DatabaseURL: true,
		Ready: func(cfg config.Config) bool {
			r := cfg.Integrations.ResolveDB
			if !r.Enabled || r.DatabaseURL == "" {
				return false
			}
			if !r.DryRun && cfg.Integrations.NodeIndexPath == "" {
				return false
			}
			return true
		},
		New: func(cfg config.Config, client *branchdam.Client) tray.IntegrationSyncer {
			r := cfg.Integrations.ResolveDB
			var edgeClient resolve.EdgeAttacher
			var vEmitter resolve.VirtualNodeEmitter
			if !r.DryRun {
				edgeClient = client
				vEmitter = client
			}
			rewrites := make([]resolve.PathRewrite, len(r.PathRewrites))
			for i, rw := range r.PathRewrites {
				rewrites[i] = resolve.PathRewrite{From: rw.From, To: rw.To}
			}
			return &resolveDBSyncer{
				client:         edgeClient,
				virtualEmitter: vEmitter,
				agentID:        cfg.AgentID,
				databaseURL:    r.DatabaseURL,
				nodeIndexPath:  cfg.Integrations.NodeIndexPath,
				dryRun:         r.DryRun,
				pathRewrites:   rewrites,
				virtualRoot:    "/virtual/resolve",
				timeout:        time.Duration(r.TimeoutSecsOrDefault()) * time.Second,
			}
		},
		Interval: func(cfg config.Config) time.Duration {
			return time.Duration(cfg.Integrations.ResolveDB.SyncIntervalMinutesOrDefault()) * time.Minute
		},
		CurrentRewrites: func(cfg config.Config) string {
			return formatResolvePathRewrites(cfg.Integrations.ResolveDB.PathRewrites)
		},
		ApplyRewrites: func(cfg *config.Config, v string) error {
			rewrites, err := parseResolvePathRewrites(v)
			if err != nil {
				return err
			}
			cfg.Integrations.ResolveDB.PathRewrites = rewrites
			return nil
		},
	},
}

// applyIntegrationBoolChange mutates cfg in place if key matches
// "integrations.<id>.enabled" or "integrations.<id>.dryRun" for a known
// integration, reporting handled=false otherwise. Centralizes the
// integrations.*.* key parsing so validateBoolChange (settings.go) doesn't
// need a per-integration case that could independently drift as lrcat
// (#47)/applephotos (#46) land -- one call site here covers every
// integration in integrationBuilders.
func applyIntegrationBoolChange(cfg *config.Config, key string, v bool) (handled bool) {
	for _, b := range integrationBuilders {
		// Match the key BEFORE deriving b.Current(*cfg) -- a Hermes
		// review nit on this PR: calling Current for every builder ahead
		// of knowing whether its key even matches is a wasted derive per
		// non-matching entry. Harmless at today's single-entry registry
		// size, but cheap to avoid outright as lrcat (#47)/applephotos
		// (#46) grow it.
		var c config.CatalogSyncConfig
		switch key {
		case b.ConfigKey("enabled"):
			c = b.Current(*cfg)
			c.Enabled = v
		case b.ConfigKey("dryRun"):
			c = b.Current(*cfg)
			c.DryRun = v
		default:
			continue
		}
		b.Apply(cfg, c)
		return true
	}
	return false
}

// applyIntegrationIntChange is applyIntegrationBoolChange's counterpart
// for "integrations.<id>.syncIntervalMinutes" and
// "integrations.<id>.timeoutSecs".
func applyIntegrationIntChange(cfg *config.Config, key string, v int) (handled bool) {
	for _, b := range integrationBuilders {
		var c config.CatalogSyncConfig
		switch key {
		case b.ConfigKey("syncIntervalMinutes"):
			c = b.Current(*cfg)
			c.SyncIntervalMinutes = v
		case b.ConfigKey("timeoutSecs"):
			c = b.Current(*cfg)
			c.TimeoutSecs = v
		default:
			continue
		}
		b.Apply(cfg, c)
		return true
	}
	return false
}

// applyIntegrationStringChange is applyIntegrationBoolChange's counterpart
// for "integrations.<id>.catalogPath" and "integrations.<id>.databaseUrl".
// integrations.nodeIndexPath is NOT handled here -- it's a shared, top-level
// IntegrationsConfig field, not per-integration, so validateStringChange's
// own switch handles it directly alongside server.baseUrl and friends.
func applyIntegrationStringChange(cfg *config.Config, key, v string) (handled bool) {
	for _, b := range integrationBuilders {
		switch key {
		case b.ConfigKey("catalogPath"):
			c := b.Current(*cfg)
			c.CatalogPath = v
			b.Apply(cfg, c)
			return true
		case b.ConfigKey("databaseUrl"):
			// ResolveDB uses DatabaseURL, mapped through CatalogPath in Current/Apply.
			c := b.Current(*cfg)
			c.CatalogPath = v
			b.Apply(cfg, c)
			return true
		}
	}
	return false
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
//
// The returned *resolveDBSyncer (if non-nil) is the concrete wiring
// target for delta-detection callbacks -- callers set prevMemberships,
// onSaveMemberships, and onSyncComplete after construction.
func buildIntegrationDeps(cfg config.Config, client *branchdam.Client) (map[tray.IntegrationID]tray.IntegrationSyncer, *resolveDBSyncer) {
	deps := make(map[tray.IntegrationID]tray.IntegrationSyncer, len(integrationBuilders))
	var resolveSyncer *resolveDBSyncer
	for _, b := range integrationBuilders {
		if b.Ready(cfg) {
			s := b.New(cfg, client)
			deps[b.ID] = s
			if rs, ok := s.(*resolveDBSyncer); ok {
				resolveSyncer = rs
			}
		}
	}
	return deps, resolveSyncer
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

// resolveDBSyncer implements tray.IntegrationSyncer over internal/resolve --
// the concrete wiring cmd/branchdam-agent owns, matching luminarSyncer's own
// relationship to internal/luminar.
//
// It stores PATHS, not open handles: resolve.Open and nodeindex.Load both
// run inside Sync and are released before it returns -- the database may be
// actively written to by DaVinci Resolve, and re-resolving the node index
// per pass means a regenerated index file is picked up without a tray
// restart. client is nil in dry-run mode, mirroring luminarSyncer's own
// treatment.
type resolveDBSyncer struct {
	client         resolve.EdgeAttacher
	virtualEmitter resolve.VirtualNodeEmitter
	agentID        string
	databaseURL    string
	nodeIndexPath  string
	dryRun         bool
	pathRewrites   []resolve.PathRewrite
	virtualRoot    string
	timeout        time.Duration
	// prevMemberships is the set from the previous successful sync pass,
	// loaded from runtime.json. Enables delta detection.
	prevMemberships []tray.SyncMembershipEntry
	// onSaveMemberships persists the current pass's emitted membership
	// set to runtime.json. Nil means no persistence (test).
	onSaveMemberships func(entries []tray.SyncMembershipEntry) error
	// onSyncComplete updates the runner's in-memory carry-forward with
	// the fresh emitted set from this pass. Called under r.mu.
	onSyncComplete func(entries []tray.SyncMembershipEntry)
}

func (s *resolveDBSyncer) Sync(ctx context.Context) (tray.SyncSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	db, err := resolve.Open(ctx, s.databaseURL)
	if err != nil {
		return tray.SyncSummary{DryRun: s.dryRun}, err
	}
	defer func() { _ = db.Close() }()

	// A dry run needs no node index at all -- every candidate simply
	// can't resolve, which the summary's Skipped count already conveys.
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

	syncer := &resolve.Syncer{
		DB:             db,
		Index:          index,
		Client:         s.client,
		VirtualEmitter: s.virtualEmitter,
		AgentID:        s.agentID,
		DatabaseURL:    s.databaseURL,
		DryRun:         s.dryRun,
		PathRewrites:   s.pathRewrites,
		VirtualRoot:    s.virtualRoot,
		PrevMemberships: trayToResolveMemberships(s.prevMemberships),
		OnSaveMemberships: func(entries []resolve.MembershipEntry) error {
			// Bridge: update runner's in-memory carry-forward with
			// the fresh emitted set, then persist to runtime.json.
			trayEntries := resolveToTrayMemberships(entries)
			if s.onSyncComplete != nil {
				s.onSyncComplete(trayEntries)
			}
			if s.onSaveMemberships != nil {
				return s.onSaveMemberships(trayEntries)
			}
			return nil
		},
	}

	stats, err := syncer.Sync(ctx)
	summary := tray.SyncSummary{
		DryRun:        s.dryRun,
		PairsFound:    stats.ClipsFound,
		Emitted:       stats.Emitted,
		Skipped:       stats.Unresolved + stats.NoRewrite,
		Errors:        stats.Errors,
		VirtualNodes:  stats.VirtualNodes,
		EdgesAttached: stats.EdgesAttached,
		Removed:       stats.Removed,
		FileMissing:   stats.FileMissing,
	}
	return summary, err
}

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
//
// A pass that outlives its own interval (e.g. syncIntervalMinutes set
// shorter than a large catalog's real sync time) cannot re-fire
// back-to-back with no cooldown: fn runs SYNCHRONOUSLY inside the select
// case, so timer.Reset is only ever called once fn has returned -- no
// tick accumulates while fn is running, and last is stamped right after,
// so the very next check sees an elapsed time near zero and correctly
// waits a full interval() before firing again (verified by
// TestStartPeriodicVarNoBackToBackAfterLongPass). Considered stamping
// last at pass START instead (a Hermes review suggestion on this PR) --
// traced through and rejected: that ordering is what WOULD introduce a
// back-to-back re-fire the moment a long pass finishes, since elapsed
// time would already exceed interval() by the time the next check runs.
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
				// Stamped AFTER fn() returns, deliberately -- see this
				// function's own doc comment on why a pass that outlives
				// its own interval still can't re-fire back-to-back: the
				// timer is only Reset (re-armed) once fn() returns, so no
				// tick accumulates during a long pass, and stamping here
				// means the very next check sees an elapsed time close to
				// 0, correctly waiting a full iv from THIS point before
				// firing again.
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

// trayToResolveMemberships converts tray.SyncMembershipEntry slice to
// resolve.MembershipEntry slice for wiring into resolve.Syncer.
func trayToResolveMemberships(in []tray.SyncMembershipEntry) []resolve.MembershipEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]resolve.MembershipEntry, len(in))
	for i, e := range in {
		out[i] = resolve.MembershipEntry{MediaPath: e.MediaPath, TimelineID: e.TimelineID}
	}
	return out
}

// resolveToTrayMemberships converts resolve.MembershipEntry slice to
// tray.SyncMembershipEntry slice for the tray callback bridge.
func resolveToTrayMemberships(in []resolve.MembershipEntry) []tray.SyncMembershipEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]tray.SyncMembershipEntry, len(in))
	for i, e := range in {
		out[i] = tray.SyncMembershipEntry{MediaPath: e.MediaPath, TimelineID: e.TimelineID}
	}
	return out
}

// formatResolvePathRewrites formats a PathRewrite slice as a
// comma-separated "from:to" string for display in the tray menu.
func formatResolvePathRewrites(rewrites []config.ResolvePathRewrite) string {
	var parts []string
	for _, rw := range rewrites {
		parts = append(parts, rw.From+":"+rw.To)
	}
	return strings.Join(parts, ", ")
}

// parseResolvePathRewrites parses a comma-separated "from:to" string into a
// ResolvePathRewrite slice. Each pair must contain exactly one colon.
func parseResolvePathRewrites(s string) ([]config.ResolvePathRewrite, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []config.ResolvePathRewrite
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.LastIndex(part, ":")
		if idx <= 0 || idx == len(part)-1 {
			return nil, fmt.Errorf("path rewrite %q must be in format from:to", part)
		}
		out = append(out, config.ResolvePathRewrite{
			From: strings.TrimSpace(part[:idx]),
			To:   strings.TrimSpace(part[idx+1:]),
		})
	}
	return out, nil
}
