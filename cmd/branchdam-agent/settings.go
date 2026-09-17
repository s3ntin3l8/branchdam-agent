package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/hooks/resolve"
	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolvehook"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// configSettings implements tray.Settings over config.Patch/config.Load --
// the concrete wiring cmd/branchdam-agent owns (internal/tray itself never
// imports internal/config, matching Ingester/SelfUpdater's existing
// pattern of interfaces defined where they're consumed, not where they're
// implemented).
type configSettings struct {
	path   string
	runner *tray.Runner

	// resolveInstaller is the DaVinci Resolve render-hook installer
	// (issue #60) registered on Runner at startup. reload() calls
	// SetScriptsDir on it when integrations.resolve.scriptsDir changes
	// and refreshes Runner.hookState via Runner.RefreshHookState -- the
	// settings-reload invalidation seam for issue #154 / audit F-17. Nil
	// in tests that don't register a hook installer; reload() short-
	// circuits on a nil installer so those tests don't need to wire one.
	resolveInstaller *resolveHookInstaller

	// appliedStatusAddr is what THIS PROCESS actually bound at launch
	// -- fixed for the whole process lifetime, unlike s.cfg below
	// (which reload() overwrites on every call). RestartRequired is
	// derived by diffing the current config against this, not against
	// the previous s.cfg snapshot: a Hermes review finding on this PR
	// caught that diffing against the mutable snapshot made the flag
	// latch permanently after the first reload, even if an operator
	// reverted a hand-edit back to the original value on a second reload
	// -- the "previous" snapshot by then was already the changed one,
	// so the diff against it saw nothing.
	appliedStatusAddr string

	mu              sync.Mutex
	cfg             config.Config
	restartRequired bool
	// queueStore is set once via SetQueueStore, right after runTrayCmd
	// opens queue.db (nil when offline.queueDbPath isn't configured).
	// reload() uses it to rebuild queueDrainer/queuePruner against the
	// freshly reloaded client/config on every settings change -- without
	// this, changing server.baseUrl or rotating server.apiKey from the
	// Settings menu would leave the drain/prune timers silently using the
	// stale client indefinitely (a Hermes review finding on this PR: the
	// queue.db *path* genuinely can't be hot-reloaded, per
	// Runner.SetQueueDeps' own doc comment, but the client/config the
	// Drainer/Pruner built from it captured at startup very much can go
	// stale).
	queueStore *queue.Store
}

// newConfigSettings builds a configSettings over the already-loaded cfg
// (runTrayCmd's own startup load -- avoids reading config.yaml twice
// before anything has changed).
func newConfigSettings(path string, cfg config.Config, runner *tray.Runner) *configSettings {
	return &configSettings{
		path:              path,
		cfg:               cfg,
		runner:            runner,
		appliedStatusAddr: cfg.Tray.StatusAddrOrDefault(),
	}
}

// SetResolveInstaller wires the DaVinci Resolve render-hook installer
// registered on Runner at startup. reload() uses it to update the
// installer's scriptsDir override and re-Detect against the new dir on
// every settings change -- the seam for issue #154 / audit F-17 (hook
// state cache refresh after settings change). Called once from
// runTrayCmd, after the installer has been created. Left nil in tests
// that don't register a hook installer; reload() short-circuits.
func (s *configSettings) SetResolveInstaller(installer *resolveHookInstaller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveInstaller = installer
}

// SetQueueStore wires the queue.db handle reload() needs to rebuild
// queueDrainer/queuePruner against a fresh client/config on every settings
// change. Called once from runTrayCmd, after queue.Open succeeds -- left
// nil (the zero value) when offline.queueDbPath isn't configured, in
// which case reload() leaves Runner's queue deps alone, same as today.
func (s *configSettings) SetQueueStore(store *queue.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueStore = store
}

// currentConfig returns the most recently loaded config -- the live source
// runTrayCmd's per-integration startPeriodicVar scheduler goroutines read
// their own interval from on every check, so a config change (from a hand
// edit + "Reload config", or a later PR's Settings menu) takes effect
// without a tray restart. Cheap: a mutex lock plus a struct copy, safe to
// call on every scheduler tick (default every 30s, see
// integrationSyncCheckInterval).
func (s *configSettings) currentConfig() config.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *configSettings) Snapshot() tray.SettingsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg

	// Built from integrationBuilders (not tray.Integrations()) since this
	// is cmd/branchdam-agent, the package that owns config.Config -- one
	// entry per builder, in registry order, matching
	// TestRegistryCompleteness's bijection guarantee against
	// tray.Integrations().
	integrations := make([]tray.IntegrationView, 0, len(integrationBuilders))
	for _, b := range integrationBuilders {
		c := b.Current(cfg)
		displayPath := c.CatalogPath
		if b.DatabaseURL && displayPath != "" {
			displayPath = databaseURLDisplay(displayPath)
		}
		var pathRewrites string
		pathRewritesSet := b.CurrentRewrites != nil
		if pathRewritesSet {
			pathRewrites = b.CurrentRewrites(cfg)
			pathRewritesSet = pathRewrites != ""
		}
		integrations = append(integrations, tray.IntegrationView{
			ID:                  b.ID,
			Title:               b.Title,
			Enabled:             c.Enabled,
			DryRun:              c.DryRun,
			CatalogPath:         displayPath,
			CatalogPathSet:      c.CatalogPath != "",
			SyncIntervalMinutes: c.SyncIntervalMinutes,
			TimeoutSecs:         c.TimeoutSecs,
			PathRewrites:        pathRewrites,
			PathRewritesSet:     pathRewritesSet,
		})
	}

	return tray.SettingsView{
		ConfigPath:                 s.path,
		StartOnLogin:               cfg.Tray.StartOnLogin,
		ConfirmDestructive:         cfg.Tray.ConfirmDestructive,
		SelfUpdateEnabled:          cfg.SelfUpdate.Enabled,
		SelfUpdateCheckIntervalHrs: cfg.SelfUpdate.CheckIntervalHours,
		RequireUnbuffered:          cfg.Ingest.RequireUnbuffered,
		RequireDCIM:                cfg.Ingest.RequireDCIM,
		PauseUploadOnMetered:       cfg.Ingest.PauseUploadOnMetered,
		AutoEject:                  cfg.Ingest.AutoEject,
		ServerBaseURL:              cfg.Server.BaseURL,
		ServerAPIKeySet:            cfg.Server.APIKey != "",
		AgentID:                    cfg.AgentID,
		ArchiveRoot:                cfg.Ingest.ArchiveRoot,
		LocalEditRoot:              cfg.Ingest.LocalEditRoot,
		NamingTemplate:             cfg.Ingest.PathTemplate,
		PathMappings:               formatPathMappings(cfg.PathMappings),
		PathMappingEntries:         toPathMappingEntries(cfg.PathMappings),
		AllowedExtensions:          cfg.Ingest.AllowedExtensions,
		CardRoots:                  cfg.Ingest.CardRoots,
		RestartRequired:            s.restartRequired,
		NodeIndexPath:              cfg.Integrations.NodeIndexPath,
		NodeIndexPathSet:           cfg.Integrations.NodeIndexPath != "",
		Integrations:               integrations,
	}
}

// databaseURLDisplay never exposes userinfo, hosts, database names, or query
// parameters on the loopback status page. The full URL remains only in the
// mode-0600 config file and the in-memory syncer.
func databaseURLDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "configured (details hidden)"
	}
	return u.Scheme + ":…"
}

func (s *configSettings) SetBool(key string, v bool) error {
	if err := s.validateBoolChange(key, v); err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	if key == "tray.startOnLogin" {
		// Best-effort, mirroring runTrayCmd's own startup registration: a
		// failure here shouldn't roll back the config change, which has
		// already been saved -- the operator's stated intent is what
		// config.yaml should reflect regardless of whether the OS
		// cooperated with actually registering the login item.
		var err error
		if v {
			err = enableStartOnLogin(s.path)
		} else {
			err = autostart.Disable()
		}
		if err != nil {
			slog.Warn("start-on-login registration change failed", "enabled", v, "err", err)
		}
	}
	return s.reload()
}

func (s *configSettings) SetInt(key string, v int) error {
	if err := s.validateIntChange(key, v); err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return s.reload()
}

func (s *configSettings) SetStringSlice(key string, v []string) error {
	if err := s.validateStringSliceChange(key, v); err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return s.reload()
}

// validateBoolChange/validateIntChange/validateStringChange each build a
// copy of the current config with one field hypothetically changed and
// run Validate() against it -- entirely in memory, before config.Patch
// ever touches disk. Without this, a bad value (an unexpanded ${VAR}
// placeholder typed into a dialog, a too-short API key) would be
// persisted to config.yaml by Patch and only THEN rejected by reload(),
// leaving the file and the running tray's in-memory config permanently
// diverged -- a Hermes review finding on this PR. Validating the
// hypothetical change first means an invalid value never reaches disk at
// all.
func (s *configSettings) validateBoolChange(key string, v bool) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "tray.startOnLogin":
		cfg.Tray.StartOnLogin = v
	case "tray.confirmDestructive":
		cfg.Tray.ConfirmDestructive = v
	case "selfUpdate.enabled":
		cfg.SelfUpdate.Enabled = v
	case "ingest.requireUnbuffered":
		cfg.Ingest.RequireUnbuffered = v
	case "ingest.requireDCIM":
		cfg.Ingest.RequireDCIM = v
	case "ingest.pauseUploadOnMetered":
		cfg.Ingest.PauseUploadOnMetered = v
	case "ingest.autoEject":
		cfg.Ingest.AutoEject = v
	default:
		// config.Patch does no schema validation of its own -- these three
		// switches (this one plus validateIntChange/validateStringChange
		// below) ARE the entire allowlist. Without a rejection here, an
		// unrecognized key (a typo in a menu handler, a stale key after a
		// rename) would silently validate an UNCHANGED cfg, report no
		// problem, and be written to config.yaml by config.Patch with no
		// validation at all -- the latent bug issue #58 fixed. Every
		// integrations.<id>.enabled / .dryRun key is handled generically
		// via applyIntegrationBoolChange (integrations.go), covering every
		// entry in integrationBuilders without a per-integration case
		// here that could independently drift as lrcat (#47)/applephotos
		// (#46) land.
		if !applyIntegrationBoolChange(&cfg, key, v) {
			return fmt.Errorf("settings: %q is not a settable bool key", key)
		}
	}
	return firstValidateProblem(cfg)
}

func (s *configSettings) validateIntChange(key string, v int) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "selfUpdate.checkIntervalHours":
		cfg.SelfUpdate.CheckIntervalHours = v
	default:
		// See validateBoolChange's default case for why this exists.
		// integrations.<id>.syncIntervalMinutes is handled generically
		// via applyIntegrationIntChange, same reasoning.
		if !applyIntegrationIntChange(&cfg, key, v) {
			return fmt.Errorf("settings: %q is not a settable int key", key)
		}
	}
	return firstValidateProblem(cfg)
}

func (s *configSettings) validateStringChange(key, v string) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "server.baseUrl":
		cfg.Server.BaseURL = v
	case "server.apiKey":
		cfg.Server.APIKey = v
	case "agentId":
		v = strings.TrimSpace(v)
		if v == "" {
			return fmt.Errorf("agentId must not be empty")
		}
		cfg.AgentID = v
	case "ingest.archiveRoot":
		cfg.Ingest.ArchiveRoot = v
	case "ingest.localEditRoot":
		cfg.Ingest.LocalEditRoot = v
	case "ingest.cardRoots":
		cfg.Ingest.CardRoots = splitCommaPaths(v)
	case "ingest.allowedExtensions":
		exts, err := splitCommaExtensions(v)
		if err != nil {
			return err
		}
		cfg.Ingest.AllowedExtensions = exts
	case "ingest.pathTemplate":
		cfg.Ingest.PathTemplate = v
	case "pathMappings":
		mappings, err := parsePathMappings(v)
		if err != nil {
			return err
		}
		cfg.PathMappings = mappings
	case "integrations.nodeIndexPath":
		// Shared across every catalog integration (see
		// config.IntegrationsConfig.NodeIndexPath's own doc comment) --
		// a top-level field, not per-integration, so it's handled
		// directly here rather than through applyIntegrationStringChange.
		cfg.Integrations.NodeIndexPath = v
	default:
		// See validateBoolChange's default case for why this exists.
		// integrations.<id>.catalogPath is handled generically via
		// applyIntegrationStringChange, same reasoning.
		if !applyIntegrationStringChange(&cfg, key, v) {
			return fmt.Errorf("settings: %q is not a settable string key", key)
		}
	}
	return firstValidateProblem(cfg)
}

func (s *configSettings) validateStringSliceChange(key string, v []string) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "ingest.autoImportPaths":
		cfg.Ingest.AutoImportPaths = append([]string(nil), v...)
	case "ingest.cardRoots":
		cfg.Ingest.CardRoots = append([]string(nil), v...)
	case "ingest.allowedExtensions":
		// The string form of this same key (validateStringChange, via
		// splitCommaExtensions) requires a leading dot on every non-empty
		// extension -- enforce the same rule here so the array wire path
		// (SetStringSlice, what the chip-list editor uses) can't silently
		// persist a value ("jpg") the string path would have rejected.
		for _, ext := range v {
			if !strings.HasPrefix(ext, ".") || len(ext) == 1 {
				return fmt.Errorf("extension %q must start with a leading dot (e.g. %q)", ext, "."+strings.TrimPrefix(ext, "."))
			}
		}
		cfg.Ingest.AllowedExtensions = append([]string(nil), v...)
	default:
		return fmt.Errorf("settings: %q is not a settable string slice key", key)
	}
	return firstValidateProblem(cfg)
}

// toPathMappingEntries converts a PathMapping slice into the tray-local
// PathMappingEntry shape the settings API and the Settings window's
// structured editor both consume -- SettingsView.PathMappingEntries'
// canonical form, alongside the legacy formatted string PathMappings.
func toPathMappingEntries(mappings []config.PathMapping) []tray.PathMappingEntry {
	if len(mappings) == 0 {
		return nil
	}
	out := make([]tray.PathMappingEntry, len(mappings))
	for i, m := range mappings {
		out[i] = tray.PathMappingEntry{WorkstationPath: m.WorkstationPath, ContainerPath: m.ContainerPath}
	}
	return out
}

// SetPathMappings replaces the whole pathMappings list -- see
// tray.Settings.SetPathMappings's own doc comment for why this is a
// separate method from SetString rather than routing through the
// "workstationPath:containerPath, ..." string format, which is lossy for
// a path containing a comma. Trims each field and rejects an entry with
// either side empty, reusing parsePathMappings' own error wording.
func (s *configSettings) SetPathMappings(entries []tray.PathMappingEntry) error {
	mappings := make([]config.PathMapping, 0, len(entries))
	for _, e := range entries {
		ws := strings.TrimSpace(e.WorkstationPath)
		cp := strings.TrimSpace(e.ContainerPath)
		if ws == "" || cp == "" {
			return fmt.Errorf("path mapping %q must be in format workstationPath:containerPath", ws+":"+cp)
		}
		mappings = append(mappings, config.PathMapping{WorkstationPath: ws, ContainerPath: cp})
	}

	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	cfg.PathMappings = mappings
	if err := firstValidateProblem(cfg); err != nil {
		return err
	}

	// mappings is built via make/append above, so it is never nil (even
	// when entries is empty) -- config.Patch's yaml.Node encoder writes an
	// empty slice as "[]", not "null", only when it isn't nil. A clear
	// therefore round-trips on disk as "configured empty," a more honest
	// representation of "the operator deliberately emptied this" than
	// "null" (unset). This does NOT change what happens next: reload()
	// (below) can still re-supply a server-provided value via
	// applyServerPathMappings, whose own guard (len(cfg.PathMappings) > 0)
	// treats a nil and an empty-non-nil slice identically -- see that
	// function's own doc comment, and the Path mappings field's UI note
	// (app.js) that surfaces this on the very save that triggers it.
	if err := config.Patch(s.path, map[string]any{"pathMappings": mappings}); err != nil {
		return fmt.Errorf("save pathMappings: %w", err)
	}
	return s.reload()
}

func firstValidateProblem(cfg config.Config) error {
	if problem := firstBlockingProblem(cfg); problem != nil {
		return fmt.Errorf("config problem: %s", problem)
	}
	return nil
}

// firstBlockingProblem returns the first Validate() Problem that should
// block a settings-driven config mutation or reload -- i.e. a structural
// failure, not a Problem marked Advisory(). Used by firstValidateProblem
// (SetBool/SetInt/SetString path) and reload (Reload config / Restart
// Required path), so they share one definition of "blocking" instead of
// each diverging independently (Hermes review finding on the PR that
// introduced SeverityWarning, issue #96).
//
// Returns nil when every Problem is advisory or when Validate() found
// nothing at all.
func firstBlockingProblem(cfg config.Config) *config.Problem {
	for _, p := range cfg.Validate() {
		if !p.Advisory() {
			return &p
		}
	}
	return nil
}

// missingRequiredFields returns the list of required config fields that
// are empty. Used by both runTrayCmd and reload to compute the
// configIncomplete flag consistently, AFTER the server handshake (if any)
// has had a chance to run -- see resolveServerConfig. Filesystem dual-write
// needs archiveRoot/pathMappings. Direct HTTP upload does not use either
// unless offline queueing is configured: its outage fallback is the existing
// archive-backed queue path and therefore still needs both.
func missingRequiredFields(cfg config.Config) []string {
	var missing []string
	if strings.TrimSpace(cfg.Server.APIKey) == "" {
		missing = append(missing, "server.apiKey")
	}
	if strings.TrimSpace(cfg.Server.BaseURL) == "" {
		missing = append(missing, "server.baseUrl")
	}
	if strings.TrimSpace(cfg.AgentID) == "" {
		missing = append(missing, "agentId")
	}
	if strings.TrimSpace(cfg.Ingest.LocalEditRoot) == "" {
		missing = append(missing, "ingest.localEditRoot")
	}
	if !cfg.Ingest.UploadStream || strings.TrimSpace(cfg.Offline.QueueDBPath) != "" {
		if strings.TrimSpace(cfg.Ingest.ArchiveRoot) == "" {
			missing = append(missing, "ingest.archiveRoot")
		}
		if len(cfg.PathMappings) == 0 {
			missing = append(missing, "pathMappings")
		}
	}
	return missing
}

// serverConfigured reports whether cfg has enough server-side information
// to attempt a real handshake. URL, API key, and agent ID are all required;
// sending a blank workstation identity would make the handshake ambiguous
// even if authentication succeeds (resolved thread PRRT_kwDOUALcu86h3ibx:
// a missing API key must route to the dummy client even when baseUrl is
// set, rather than attempting -- and silently failing -- a real dial).
func serverConfigured(cfg config.Config) bool {
	return strings.TrimSpace(cfg.Server.BaseURL) != "" &&
		strings.TrimSpace(cfg.Server.APIKey) != "" &&
		strings.TrimSpace(cfg.AgentID) != ""
}

// resolveServerConfig is the single source of truth for the
// "construct a client, maybe handshake, compute what's still missing"
// sequence shared by runTrayCmd (startup) and reload() (Settings menu).
// It deliberately runs the handshake -- and therefore
// applyServerPathMappings -- BEFORE computing missingFields: a fresh
// install's config.yaml has baseUrl/apiKey/agentId/archiveRoot/localEditRoot set
// but an empty pathMappings, and the server handshake is the only way
// that gap gets filled. Computing missingFields first (the pre-#185-fix
// order) made configIncomplete true before the handshake ever had a
// chance to run, which in turn skipped the handshake entirely (it's
// gated on !configIncomplete) -- a chicken-and-egg deadlock that made
// applyServerPathMappings unreachable on the only path that needs it.
//
// hsTimeout bounds the handshake call; warnPrefix distinguishes the
// startup vs. reload log lines without duplicating the surrounding code.
func resolveServerConfig(ctx context.Context, cfg *config.Config, configPath string, hsTimeout time.Duration, warnPrefix string) (client *branchdam.Client, missingFields []string) {
	if serverConfigured(*cfg) {
		client = branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)

		hsCtx, hsCancel := context.WithTimeout(ctx, hsTimeout)
		if hs, err := client.Handshake(hsCtx, branchdam.HandshakeRequest{AgentID: cfg.AgentID}); err != nil {
			slog.Warn("could not sync naming template from server handshake"+warnPrefix+"; using config value", "err", err)
		} else {
			if hs.NamingTemplate != "" {
				cfg.Ingest.PathTemplate = hs.NamingTemplate
			}
			applyServerPathMappings(cfg, *hs, configPath)
		}
		hsCancel()
	} else {
		// Dummy client -- the tray starts (or continues) in "not
		// configured" mode and ingest/drain/prune are blocked via
		// missingFields below.
		client = branchdam.New("http://localhost:1", "")
	}

	return client, missingRequiredFields(*cfg)
}

// applyServerPathMappings applies server-provided path mappings to the config
// when the agent has none configured yet. This is the single source of truth
// for the handshake → config path-mapping sync, used by both runTrayCmd and
// reload. Server wins when client has none; local mappings are never overwritten.
//
// "Has none" is a length check (len(cfg.PathMappings) > 0 below), so a nil
// slice and a deliberately-emptied non-nil one (SetPathMappings clearing
// the last row) are indistinguishable to this guard -- an operator who
// clears the list via the Settings window's structured editor will see it
// re-populated on the very next reload if hs.PathMappings is non-empty
// for their agent (Hermes review finding on the PR that added
// SetPathMappings). There is no way to represent "explicitly emptied,
// don't re-supply" in config.yaml today; that's tracked separately
// (issue #234 also covers hs.PathMappings itself currently always being
// empty in practice, since the server doesn't populate it -- which is
// the only reason this re-supply path doesn't bite today).
//
// Trust boundary note: this is intentional trust-of-server, matching the
// agent's existing trust model for NamingTemplate and other server-pushed
// config. The client signs outgoing requests (HMAC on X-Timestamp/X-Nonce)
// but does not verify server response signatures — the server is the source
// of truth wholesale. The slog.Info line below lists applied prefixes for
// operator audit.
func applyServerPathMappings(cfg *config.Config, hs branchdam.HandshakeResponse, configPath string) {
	if len(hs.PathMappings) == 0 || len(cfg.PathMappings) > 0 {
		return
	}
	cfg.PathMappings = make([]config.PathMapping, len(hs.PathMappings))
	for i, pm := range hs.PathMappings {
		cfg.PathMappings[i] = config.PathMapping{
			WorkstationPath: pm.WorkstationPrefix,
			ContainerPath:   pm.ContainerPath,
		}
	}
	prefixes := make([]string, len(cfg.PathMappings))
	for i, pm := range cfg.PathMappings {
		prefixes[i] = pm.WorkstationPath
	}
	slog.Info("applied path mappings from server handshake", "count", len(cfg.PathMappings), "workstationPrefixes", prefixes)
	// Persist to config.yaml so the mappings survive a restart.
	if err := config.Patch(configPath, map[string]any{"pathMappings": cfg.PathMappings}); err != nil {
		slog.Warn("could not persist server-provided path mappings to config", "err", err)
	}
}

// patchValueForStringKey converts a raw string value into the shape
// config.Patch expects for the keys validateStringChange treats specially
// (comma-separated lists, path mappings) -- every other key patches the
// string value verbatim.
func patchValueForStringKey(key, value string) (any, error) {
	switch key {
	case "ingest.cardRoots":
		return splitCommaPaths(value), nil
	case "ingest.allowedExtensions":
		return splitCommaExtensions(value)
	case "pathMappings":
		return parsePathMappings(value)
	case "agentId":
		// validateStringChange trims agentId before validating (a
		// surrounding-whitespace-only value must not read as "set"); patch
		// the same trimmed form here so a validated value and a persisted
		// value can never differ (Hermes review finding on this PR).
		return strings.TrimSpace(value), nil
	default:
		return value, nil
	}
}

// validateAndPatchString runs validateStringChange then patches key's
// on-disk value, without reloading -- SetString calls s.reload() itself
// exactly once after this succeeds.
func (s *configSettings) validateAndPatchString(key, value string) error {
	if err := s.validateStringChange(key, value); err != nil {
		return err
	}
	patchVal, err := patchValueForStringKey(key, value)
	if err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: patchVal}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return nil
}

// SetString persists one dotted config key -- see tray.Settings.SetString's
// own doc comment.
func (s *configSettings) SetString(key, value string) error {
	if err := s.validateAndPatchString(key, value); err != nil {
		return err
	}
	return s.reload()
}

// SetIntegrationPath sets a per-integration catalog path -- same
// catalogPath/databaseUrl key resolution for every integration, no dialog.
func (s *configSettings) SetIntegrationPath(id tray.IntegrationID, value string) error {
	b, ok := builderFor(id)
	if !ok {
		return fmt.Errorf("settings: unknown integration %q", id)
	}
	pathKey := "catalogPath"
	if b.ID == tray.IntegrationResolveDB {
		pathKey = "databaseUrl"
	}
	return s.SetString(b.ConfigKey(pathKey), value)
}

// parseAndValidateRewrites parses value into path rewrite rules and
// validates the result against cfg's own copy (applying the parsed rules
// to Integrations.ResolveDB.PathRewrites before running
// firstBlockingProblem, so validation sees the post-change state, not the
// stale snapshot) -- used by SetIntegrationRewrites.
func parseAndValidateRewrites(cfg config.Config, value string) ([]config.ResolvePathRewrite, error) {
	rewrites, err := parseResolvePathRewrites(value)
	if err != nil {
		return nil, err
	}
	cfgForValidation := cfg
	cfgForValidation.Integrations.ResolveDB.PathRewrites = rewrites
	if problem := firstBlockingProblem(cfgForValidation); problem != nil {
		return nil, fmt.Errorf("config problem: %s", problem)
	}
	return rewrites, nil
}

// SetIntegrationRewrites sets path rewrite rules for the given
// integration. Path rewrites are not reachable through
// SetString/validateStringChange at all (applyIntegrationStringChange only
// handles catalogPath/databaseUrl), so this uses parseAndValidateRewrites
// directly instead.
func (s *configSettings) SetIntegrationRewrites(id tray.IntegrationID, value string) error {
	b, ok := builderFor(id)
	if !ok || b.ApplyRewrites == nil {
		return fmt.Errorf("settings: integration %q does not support path rewrites", id)
	}

	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	rewrites, err := parseAndValidateRewrites(cfg, value)
	if err != nil {
		return err
	}

	key := b.ConfigKey("pathRewrites")
	if err := config.Patch(s.path, map[string]any{key: rewrites}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return s.reload()
}

// reload re-reads config.yaml, rebuilds the branchdam.Client and
// ingest.Engine it feeds, applies them via Runner.Reconfigure, and --
// when SetQueueStore wired a queue.db handle -- also rebuilds
// queueDrainer/queuePruner against the same fresh client/config and
// re-applies them via Runner.SetQueueDeps. Without this second half, a
// server.baseUrl/server.apiKey change from the Settings menu would leave
// the tray's drain/prune timers using the stale client indefinitely (a
// Hermes review finding on this PR) -- TriggerDrain/TriggerPrune read
// Runner's drainer/pruner fields fresh on every call, so the very next
// timer tick picks up the rebuilt ones automatically.
//
// Rejects on ANY Validate() problem, not just a server.*-prefixed one:
// unlike runTrayCmd's own startup gate (which only treats server.* as
// fatal, since non-server fields are advisory-only there, matching
// preflight's WARN treatment), every field this menu can edit via dialog
// (ingest.archiveRoot/localEditRoot/pathTemplate included) is something a
// typo could hit, and this is a live config-mutation path specifically
// trying to keep bad values out -- a Hermes review finding on this PR.
// This is really a backstop for a hand-edited config.yaml reaching
// "Reload config": SetBool/SetInt/SetString already validate their
// specific change before ever calling config.Patch, so a menu-driven
// change should never reach this rejection in practice.
//
// RestartRequired is re-derived by diffing against appliedStatusAddr
// (fixed at construction -- what THIS PROCESS actually bound), not against
// the mutable previous s.cfg snapshot -- see that field's own doc comment
// for why that distinction matters.
func (s *configSettings) reload() error {
	newCfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("reload config %q: %w", s.path, err)
	}
	if problem := firstBlockingProblem(newCfg); problem != nil {
		return fmt.Errorf("config problem: %s", problem)
	}
	if newCfg.Offline.QueueDBPath != "" && newCfg.Offline.Tier0ContainerRoot == "" {
		return fmt.Errorf("offline.tier0ContainerRoot must be set in config when offline.queueDbPath is set")
	}

	// Create the client, handshake if the server side is configured (issue
	// #86), and compute which required fields are still missing -- in
	// that order, so a handshake that supplies pathMappings is reflected
	// in missingFields below. See resolveServerConfig's doc comment for
	// why the order matters. Handshake failure must not block settings
	// reload -- continue with config-file values.
	client, missingFields := resolveServerConfig(context.Background(), &newCfg, s.path, 5*time.Second, " on reload")
	configIncomplete := len(missingFields) > 0

	engine := ingest.NewEngine(client, newCfg.AgentID, newCfg.Ingest, newCfg.PathMappings)

	s.mu.Lock()
	oldResolveScriptsDir := s.cfg.Integrations.Resolve.ScriptsDir
	s.cfg = newCfg
	s.restartRequired = s.appliedStatusAddr != newCfg.Tray.StatusAddrOrDefault()
	queueStore := s.queueStore
	resolveInstaller := s.resolveInstaller
	s.mu.Unlock()

	if queueStore != nil {
		engine.Queue = queueStore
		engine.Tier0ContainerRoot = newCfg.Offline.Tier0ContainerRoot
	}

	s.runner.SetArchiveRoot(newCfg.Ingest.ArchiveRoot)
	s.runner.SetArchiveProber(func(pctx context.Context, root string) bool {
		return probeArchive(pctx, root, client, newCfg.Ingest.UploadStream)
	})
	// Rebuild the server probe against the freshly reloaded client, same
	// reasoning as the integration syncers below (issue #57): rotating
	// server.apiKey or server.baseUrl from the Settings window must not
	// leave "Test connection" probing a stale client indefinitely.
	registerServerProbe(s.runner, client, serverConfigured(newCfg))
	s.runner.SetDetectorInterval(time.Duration(newCfg.Ingest.PollIntervalSecs) * time.Second)
	s.runner.SetDetectorRequireDCIM(newCfg.Ingest.RequireDCIM)
	s.runner.SetPauseUploadOnMetered(newCfg.Ingest.PauseUploadOnMetered)
	s.runner.SetAutoEject(newCfg.Ingest.AutoEject)
	// Closes a pre-existing gap surfaced while removing this menu item's own
	// tray duplicate (issue #211): tray.confirmDestructive previously only
	// ever reached the live Runner via settingsmenu.go's dispatch calling
	// SetConfirmDestructive directly, alongside the SetBool save -- a path
	// the Wails Settings window (Track 3d, PR #210) never had, since it
	// only ever calls the generic SetBool/reload path below. Toggling this
	// checkbox from the window persisted to config.yaml correctly but never
	// took live effect until the next tray restart. Now reload() re-seeds
	// it the same way every other live-tunable already does.
	s.runner.SetConfirmDestructive(newCfg.Tray.ConfirmDestructive)
	s.runner.SetConfigIncomplete(configIncomplete, missingFields)
	s.runner.Reconfigure(engine, newCfg.Ingest.CardRoots, newCfg.Ingest.LocalEditRoot)

	// Rebuild every integration syncer against the freshly reloaded
	// client/config, for the exact reason the queueDrainer/queuePruner
	// rebuild below exists (issue #57): TriggerSync reads Runner's
	// syncers map fresh on every call, so without this, rotating
	// server.apiKey or changing server.baseUrl from the menu would leave
	// every enabled integration POSTing edges with the stale client
	// indefinitely -- silently, since a 401 on an EVENT_EDGE_ATTACHED
	// surfaces only as SyncSummary.Errors, not a visible failure.
	syncers, resolveSyncer := buildIntegrationDeps(newCfg, client)
	s.runner.SetIntegrationSyncers(syncers)
	if resolveSyncer != nil {
		// Re-wire the freshly-built resolve syncer against the runtime
		// state file. Without this, a settings reload (e.g. apiKey or
		// baseUrl rotation) would install a fresh resolveDBSyncer
		// with nil prevMemberships/onSaveMemberships: delta detection
		// and persistence silently stop until process restart -- first
		// pass re-emits every edge as new and nothing is persisted.
		wireResolveSyncer(s.runner, resolveSyncer)
	}

	// Hook-state cache refresh on settings change (issue #154 / audit
	// F-17): if the operator edited integrations.resolve.scriptsDir (the
	// only field the DaVinci Resolve hook installer reads from config),
	// re-Detect against the new candidate dirs and seed Runner.hookState
	// with the fresh snapshot -- otherwise the status page's "installed
	// and up to date" / "not installed" line keeps showing the prior
	// detect's result until the next tray restart, even though the
	// installer's own view has changed. Also push the new scriptsDir
	// into the installer so a subsequent TriggerHookInstall targets the
	// new directory rather than the startup-captured one. Set first,
	// then read candidateDirs() from the installer (single source of
	// truth) -- avoids a parallel resolveHookCandidateDirs call here
	// that could drift from the installer's own view if scriptsDir
	// handling ever grows a normalization step.
	if resolveInstaller != nil && newCfg.Integrations.Resolve.ScriptsDir != oldResolveScriptsDir {
		resolveInstaller.SetScriptsDir(newCfg.Integrations.Resolve.ScriptsDir)
		detected := resolvehook.Detect(resolveInstaller.candidateDirs(), resolve.FileName, resolve.SourceSHA256)
		s.runner.RefreshHookState(tray.HookResolve, tray.HookState{
			At:        time.Now(),
			Dir:       detected.Dir,
			Path:      detected.Path,
			Installed: detected.Installed,
			UpToDate:  detected.UpToDate,
		})
	}

	if queueStore != nil {
		var drainer tray.Drainer = &queueDrainer{client: client, store: queueStore, agentID: newCfg.AgentID}
		var pruner tray.Pruner
		if newCfg.Prune.Enabled {
			pruner = &queuePruner{client: client, store: queueStore, cfg: newCfg}
		}
		s.runner.SetQueueDeps(&queueCountsReader{store: queueStore}, drainer, pruner)
	}
	return nil
}

func (s *configSettings) Reload() error {
	return s.reload()
}

func (s *configSettings) OpenConfigFile() error {
	return openWithDefaultApp(s.path)
}

func (s *configSettings) RevealConfigFolder() error {
	return openWithDefaultApp(filepath.Dir(s.path))
}

// openWithDefaultApp shells out to the platform's own "open" command.
// Kept here rather than in internal/tray since this file's only reason to
// exist is wiring internal/tray.Settings, not sharing OS-shell-out helpers
// across an internal/cmd package boundary for a two-line function.
func openWithDefaultApp(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// splitCommaPaths parses a comma-separated string of directories, trimming
// whitespace and dropping empty segments.
func splitCommaPaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// splitCommaExtensions parses a comma-separated string of file extensions,
// trimming whitespace and verifying that each non-empty extension starts
// with a leading dot. An empty or all-whitespace input returns an empty slice
// without error (meaning all extensions are allowed).
func splitCommaExtensions(s string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(s, ",") {
		ext := strings.TrimSpace(p)
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") || len(ext) == 1 {
			return nil, fmt.Errorf("extension %q must start with a leading dot (e.g. %q)", ext, "."+strings.TrimPrefix(ext, "."))
		}
		out = append(out, ext)
	}
	return out, nil
}

// formatPathMappings renders a PathMapping slice as a comma-separated
// "workstationPath:containerPath" string for the Settings menu display.
func formatPathMappings(mappings []config.PathMapping) string {
	var parts []string
	for _, m := range mappings {
		parts = append(parts, m.WorkstationPath+":"+m.ContainerPath)
	}
	return strings.Join(parts, ", ")
}

// parsePathMappings parses a comma-separated "workstationPath:containerPath"
// string into a PathMapping slice. Each pair must contain exactly one colon.
func parsePathMappings(s string) ([]config.PathMapping, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []config.PathMapping
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Use LastIndex to handle Windows drive letters (C:\path:/container).
		// The last colon separates workstation path from container path.
		idx := strings.LastIndex(part, ":")
		if idx <= 0 || idx == len(part)-1 {
			return nil, fmt.Errorf("path mapping %q must be in format workstationPath:containerPath", part)
		}
		out = append(out, config.PathMapping{
			WorkstationPath: strings.TrimSpace(part[:idx]),
			ContainerPath:   strings.TrimSpace(part[idx+1:]),
		})
	}
	return out, nil
}
