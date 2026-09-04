package main

import (
	"context"
	"fmt"
	"log/slog"
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

// configSettings implements tray.Settings over config.Patch/config.Load
// plus a dialogRunner for the five free-text fields -- the concrete
// wiring cmd/branchdam-agent owns (internal/tray itself never imports
// internal/config or knows about zenity, matching Ingester/SelfUpdater's
// existing pattern of interfaces defined where they're consumed, not
// where they're implemented).
type configSettings struct {
	path   string
	runner *tray.Runner
	dialog dialogRunner

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
func newConfigSettings(path string, cfg config.Config, runner *tray.Runner, dialog dialogRunner) *configSettings {
	return &configSettings{
		path:              path,
		cfg:               cfg,
		runner:            runner,
		dialog:            dialog,
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
		integrations = append(integrations, tray.IntegrationView{
			ID:                  b.ID,
			Enabled:             c.Enabled,
			DryRun:              c.DryRun,
			CatalogPath:         c.CatalogPath,
			CatalogPathSet:      c.CatalogPath != "",
			SyncIntervalMinutes: c.SyncIntervalMinutes,
		})
	}

	return tray.SettingsView{
		ConfigPath:                 s.path,
		StartOnLogin:               cfg.Tray.StartOnLogin,
		SelfUpdateEnabled:          cfg.SelfUpdate.Enabled,
		SelfUpdateCheckIntervalHrs: cfg.SelfUpdate.CheckIntervalHours,
		RequireUnbuffered:          cfg.Ingest.RequireUnbuffered,
		RequireDCIM:                cfg.Ingest.RequireDCIM,
		PauseUploadOnMetered:       cfg.Ingest.PauseUploadOnMetered,
		ServerBaseURL:              cfg.Server.BaseURL,
		ServerAPIKeySet:            cfg.Server.APIKey != "",
		ArchiveRoot:                cfg.Ingest.ArchiveRoot,
		LocalEditRoot:              cfg.Ingest.LocalEditRoot,
		NamingTemplate:             cfg.Ingest.PathTemplate,
		AllowedExtensions:          cfg.Ingest.AllowedExtensions,
		RestartRequired:            s.restartRequired,
		NodeIndexPath:              cfg.Integrations.NodeIndexPath,
		NodeIndexPathSet:           cfg.Integrations.NodeIndexPath != "",
		Integrations:               integrations,
	}
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
	case "selfUpdate.enabled":
		cfg.SelfUpdate.Enabled = v
	case "ingest.requireUnbuffered":
		cfg.Ingest.RequireUnbuffered = v
	case "ingest.requireDCIM":
		cfg.Ingest.RequireDCIM = v
	case "ingest.pauseUploadOnMetered":
		cfg.Ingest.PauseUploadOnMetered = v
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
		cfg.Ingest.AllowedExtensions = append([]string(nil), v...)
	default:
		return fmt.Errorf("settings: %q is not a settable string slice key", key)
	}
	return firstValidateProblem(cfg)
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
// (SetBool/SetInt/PromptAndSet path) and reload (Reload config / Restart
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

// settingsPrompt describes one PromptAndSet field's dialog.
type settingsPrompt struct {
	key     string
	kind    string // dialog.go's -kind: "entry", "password", "directory", or "file"
	title   string
	message string
	// defaultValue pre-fills an "entry" or "file" dialog with the current
	// value -- never used for "password" (a secret has no business
	// appearing, even partially, in a process's argv) or "directory".
	defaultValue func(cfg config.Config) string
	// patterns is -kind file's filename filter (e.g. {"*.json"}); unused
	// for every other kind.
	patterns []string
}

func settingsPromptFor(field tray.SettingsField) (settingsPrompt, error) {
	switch field {
	case tray.FieldServerBaseURL:
		return settingsPrompt{
			key: "server.baseUrl", kind: "entry", title: "branchDAM Server URL",
			message:      "branchDAM server URL:",
			defaultValue: func(cfg config.Config) string { return cfg.Server.BaseURL },
		}, nil
	case tray.FieldServerAPIKey:
		return settingsPrompt{
			key: "server.apiKey", kind: "password", title: "branchDAM Agent API Key",
			message: "Agent API key (from your branchDAM server, 32+ characters):",
		}, nil
	case tray.FieldArchiveRoot:
		return settingsPrompt{
			key: "ingest.archiveRoot", kind: "directory", title: "Select the archive (NAS) folder",
		}, nil
	case tray.FieldLocalEditRoot:
		return settingsPrompt{
			key: "ingest.localEditRoot", kind: "directory", title: "Select the local edit (scratch) folder",
		}, nil
	case tray.FieldCardRoots:
		return settingsPrompt{
			key: "ingest.cardRoots", kind: "entry", title: "Watch Folders",
			message: "Directories polled for mounted camera cards (comma-separated):",
			defaultValue: func(cfg config.Config) string {
				return strings.Join(cfg.Ingest.CardRoots, ", ")
			},
		}, nil
	case tray.FieldAllowedExtensions:
		return settingsPrompt{
			key: "ingest.allowedExtensions", kind: "entry", title: "Allowed Extensions",
			message: "File extensions to ingest (comma-separated, e.g. .arw, .jpg, or empty for all):",
			defaultValue: func(cfg config.Config) string {
				return strings.Join(cfg.Ingest.AllowedExtensions, ", ")
			},
		}, nil
	case tray.FieldNamingTemplate:
		return settingsPrompt{
			key: "ingest.pathTemplate", kind: "entry", title: "Naming Template",
			message:      "Destination path template ({yyyy}/{mm}/{dd}/{camera_model}/{original_name}):",
			defaultValue: func(cfg config.Config) string { return cfg.Ingest.PathTemplate },
		}, nil
	case tray.FieldNodeIndexPath:
		return settingsPrompt{
			key: "integrations.nodeIndexPath", kind: "file", title: "Select the node-index JSON file",
			patterns:     []string{"*.json"},
			defaultValue: func(cfg config.Config) string { return cfg.Integrations.NodeIndexPath },
		}, nil
	default:
		return settingsPrompt{}, fmt.Errorf("settings: unknown field %v", field)
	}
}

func (s *configSettings) PromptAndSet(field tray.SettingsField) (bool, error) {
	prompt, err := settingsPromptFor(field)
	if err != nil {
		return false, err
	}

	args := []string{"-kind", prompt.kind, "-title", prompt.title}
	if prompt.message != "" {
		args = append(args, "-message", prompt.message)
	}
	if prompt.defaultValue != nil {
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		if def := prompt.defaultValue(cfg); def != "" {
			args = append(args, "-default", def)
		}
	}
	if len(prompt.patterns) > 0 {
		args = append(args, "-patterns", strings.Join(prompt.patterns, ","))
	}

	value, exitCode, err := s.dialog(context.Background(), args...)
	if err != nil {
		return false, fmt.Errorf("run settings dialog for %s: %w", prompt.key, err)
	}
	switch exitCode {
	case dialogExitCanceled:
		return false, nil
	case dialogExitOK:
		// fall through
	default:
		return false, fmt.Errorf("settings dialog for %s failed (exit %d)", prompt.key, exitCode)
	}

	if err := s.validateStringChange(prompt.key, value); err != nil {
		return false, err
	}
	var patchVal any = value
	switch prompt.key {
	case "ingest.cardRoots":
		patchVal = splitCommaPaths(value)
	case "ingest.allowedExtensions":
		exts, _ := splitCommaExtensions(value)
		patchVal = exts
	}
	if err := config.Patch(s.path, map[string]any{prompt.key: patchVal}); err != nil {
		return false, fmt.Errorf("save %s: %w", prompt.key, err)
	}
	return true, s.reload()
}

// PromptAndSetIntegrationPath is PromptAndSet's counterpart for a
// per-integration catalog path (see tray.Settings.PromptAndSetIntegrationPath's
// own doc comment for why this is a parameterized method rather than one
// SettingsField enum value per integration). Mirrors PromptAndSet's own
// dialog/validate/patch/reload shape closely, differing only in how the
// dotted key and dialog metadata are derived (from integrationBuilders via
// id, rather than settingsPromptFor via a SettingsField).
func (s *configSettings) PromptAndSetIntegrationPath(id tray.IntegrationID) (bool, error) {
	b, ok := builderFor(id)
	if !ok {
		return false, fmt.Errorf("settings: unknown integration %q", id)
	}

	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	args := []string{"-kind", "file", "-title", "Select the " + b.Title + " catalog file"}
	if def := b.Current(cfg).CatalogPath; def != "" {
		args = append(args, "-default", def)
	}
	if len(b.CatalogFilePatterns) > 0 {
		args = append(args, "-patterns", strings.Join(b.CatalogFilePatterns, ","))
	}

	value, exitCode, err := s.dialog(context.Background(), args...)
	key := b.ConfigKey("catalogPath")
	if err != nil {
		return false, fmt.Errorf("run settings dialog for %s: %w", key, err)
	}
	switch exitCode {
	case dialogExitCanceled:
		return false, nil
	case dialogExitOK:
		// fall through
	default:
		return false, fmt.Errorf("settings dialog for %s failed (exit %d)", key, exitCode)
	}

	if err := s.validateStringChange(key, value); err != nil {
		return false, err
	}
	if err := config.Patch(s.path, map[string]any{key: value}); err != nil {
		return false, fmt.Errorf("save %s: %w", key, err)
	}
	return true, s.reload()
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
// "Reload config": SetBool/SetInt/PromptAndSet already validate their
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

	client := branchdam.New(newCfg.Server.BaseURL, newCfg.Server.APIKey)

	// Synchronize naming template from server handshake if available (issue #86).
	// Handshake failure must not block settings reload -- continue with config-file template.
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if hs, err := client.Handshake(hsCtx, branchdam.HandshakeRequest{AgentID: newCfg.AgentID}); err != nil {
		slog.Warn("could not sync naming template from server handshake on reload; using config value", "err", err)
	} else if hs.NamingTemplate != "" {
		newCfg.Ingest.PathTemplate = hs.NamingTemplate
	}
	hsCancel()

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
	s.runner.SetDetectorInterval(time.Duration(newCfg.Ingest.PollIntervalSecs) * time.Second)
	s.runner.SetDetectorRequireDCIM(newCfg.Ingest.RequireDCIM)
	s.runner.SetPauseUploadOnMetered(newCfg.Ingest.PauseUploadOnMetered)
	s.runner.Reconfigure(engine, newCfg.Ingest.CardRoots, newCfg.Ingest.LocalEditRoot)

	// Rebuild every integration syncer against the freshly reloaded
	// client/config, for the exact reason the queueDrainer/queuePruner
	// rebuild below exists (issue #57): TriggerSync reads Runner's
	// syncers map fresh on every call, so without this, rotating
	// server.apiKey or changing server.baseUrl from the menu would leave
	// every enabled integration POSTing edges with the stale client
	// indefinitely -- silently, since a 401 on an EVENT_EDGE_ATTACHED
	// surfaces only as SyncSummary.Errors, not a visible failure.
	s.runner.SetIntegrationSyncers(buildIntegrationDeps(newCfg, client))

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

// openWithDefaultApp shells out to the platform's own "open" command --
// same pattern as internal/tray/run_supported.go's openBrowser, duplicated
// rather than exported from there since this file's only reason to exist
// is wiring internal/tray.Settings, not sharing OS-shell-out helpers
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
