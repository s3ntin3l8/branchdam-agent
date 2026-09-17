package tray

// SettingsView is a read-only snapshot of the on-disk configuration
// fields the settings menu renders. Booleans/enums render as native
// checkboxes/submenus directly from these fields; the free-text fields
// are edited via Settings.SetString instead.
type SettingsView struct {
	ConfigPath string

	StartOnLogin       bool
	ConfirmDestructive bool

	SelfUpdateEnabled bool
	// SelfUpdateCheckIntervalHrs mirrors config.SelfUpdateConfig.CheckIntervalHours
	// verbatim: 0 means unset (defaults to 24 elsewhere), negative means
	// "never re-check after the initial one."
	SelfUpdateCheckIntervalHrs int

	RequireUnbuffered    bool
	RequireDCIM          bool
	PauseUploadOnMetered bool
	AutoEject            bool

	ServerBaseURL   string
	ServerAPIKeySet bool

	AgentID       string
	ArchiveRoot   string
	LocalEditRoot string
	// NamingTemplate is overwritten by the branchDAM server's handshake
	// response on every tray startup/reload whenever the server returns a
	// non-empty value (cmd/branchdam-agent/settings.go's resolveServerConfig)
	// -- which, per the server's own agent-handshake handler, it always
	// does (falling back to naming.DefaultPathTemplate when unset). The
	// overwrite is in-memory only: an operator's own edit survives in
	// config.yaml but is shadowed by this field on the very next reload.
	// See AGENTS.md invariant 17(d). Settings windows render this
	// read-only rather than editable for that reason.
	NamingTemplate string
	// PathMappingEntries is the canonical, editable representation of
	// pathMappings -- the "workstationPath:containerPath, ..." comma/colon
	// string format was retired (issue #236): it silently corrupted any
	// path containing a comma, and every consumer (the Wails Settings
	// window, this struct) had already moved to the structured form.
	PathMappingEntries []PathMappingEntry
	AllowedExtensions  []string
	// CardRoots are the parent directories the ingest engine watches for
	// newly mounted removable card volumes (config.IngestConfig.CardRoots).
	// An empty slice means auto-detection is off (AGENTS.md invariant 14's
	// "empty roots early-returns without starting a [detector]" path), not
	// "unset" -- there is no separate CardRootsSet companion field the way
	// ServerAPIKeySet/CatalogPathSet exist for secrets, since there is
	// nothing here to mask.
	CardRoots []string

	// RestartRequired is true once a change to a restart-only field
	// (tray.statusAddr) has been saved but not yet
	// applied -- see Runner.Reconfigure's doc comment for why that
	// can't be hot-reloaded. The menu surfaces "Restart now" rather than
	// silently pretending the change already took effect.
	RestartRequired bool

	// NodeIndexPath/NodeIndexPathSet mirror ServerBaseURL/ServerAPIKeySet's
	// own pattern -- the path itself is not a secret (unlike the API key),
	// but a bool-plus-value pair keeps this field's rendering symmetric
	// with every other free-text field the menu shows a "(configured)" /
	// "(not set)" title for.
	NodeIndexPath    string
	NodeIndexPathSet bool

	// Integrations is CONFIG state only, ordered to match the compile-time
	// Integrations() registry -- paired with Runner's own
	// Status.Integrations (RUNTIME state: last sync, registered) by the
	// integrations menu to render "enabled but not configured" vs. "ready"
	// vs. a real last-sync summary. An implementation MUST emit one entry
	// per registry entry, so Integration(id) below can never miss.
	Integrations []IntegrationView
}

// IntegrationView is one catalog integration's CONFIG state -- the
// Settings-side counterpart to Runner's IntegrationStatus (RUNTIME state).
// Neither is derivable from the other; the integrations menu joins them by
// ID.
//
// CatalogPathSet is a bool alongside the path itself for the same reason
// ServerAPIKeySet exists on SettingsView: the menu's title only ever needs
// "(configured)" vs. "(not set)", and the path itself is still pre-filled
// into the Wails window's file picker via its own SetIntegrationPath-backed
// form field, not read from here.
type IntegrationView struct {
	ID IntegrationID
	// Title is the friendly display name ("Luminar Neo", "DaVinci Resolve")
	// -- IntegrationStatus's own Title/DisplayName counterpart (see that
	// struct's doc comment), but no DisplayName method here: unlike
	// IntegrationStatus, no Go code renders an IntegrationView directly --
	// it's built by Snapshot() and serialized straight to JSON, untagged
	// (this struct carries no json: tags at all, matching SettingsView), so
	// the Settings form's own JS falls back to iv.Title || iv.ID exactly
	// the way renderIntegrations/renderHooks already do for the live-status
	// side (app.js). Empty for an IntegrationView built directly in a test
	// literal, same fallback-on-empty convention as Title elsewhere in this
	// package.
	Title          string
	Enabled        bool
	DryRun         bool
	CatalogPath    string
	CatalogPathSet bool
	// SyncIntervalMinutes mirrors config.CatalogSyncConfig.SyncIntervalMinutes
	// verbatim: 0 means unset (defaults to 60 elsewhere), negative means
	// "manual only" -- same convention as SettingsView.SelfUpdateCheckIntervalHrs.
	SyncIntervalMinutes int
	// TimeoutSecs bounds one sync pass. 0 means default (30s).
	TimeoutSecs int
	// PathRewrites is the formatted "from:to, from:to" string for the
	// integration's path rewrite rules. Empty when no rules are configured.
	PathRewrites    string
	PathRewritesSet bool
}

// PathMappingEntry is one workstation-path -> container-path translation
// rule (config.PathMapping's tray-local counterpart). Declared here rather
// than imported from internal/config: this package is deliberately kept
// free of a config dependency (SettingsView is plain scalars/slices
// throughout), and this same type doubles as the request body shape for
// the settings API's path-mappings route, one shape in both directions.
type PathMappingEntry struct {
	WorkstationPath string `json:"workstationPath"`
	ContainerPath   string `json:"containerPath"`
}

// Integration looks up v's entry for id by ID, never by slice position --
// a future integration (lrcat #47, applephotos #46) could register in any
// order relative to another. ok is false if the implementation didn't
// emit an entry for id at all (a Settings/Snapshot bug, not a normal
// runtime state).
func (v SettingsView) Integration(id IntegrationID) (IntegrationView, bool) {
	for _, iv := range v.Integrations {
		if iv.ID == id {
			return iv, true
		}
	}
	return IntegrationView{}, false
}

// Settings is the subset of the tray's on-disk configuration a settings
// menu can read, toggle, and (for free-text fields) prompt for and change
// at runtime -- an interface, like Ingester and SelfUpdater, so this
// package's own tests never touch a real config.yaml, dialog backend, or
// filesystem.
type Settings interface {
	Snapshot() SettingsView

	// SetBool/SetInt/SetStringSlice persist one dotted config key (e.g.
	// "tray.startOnLogin", "selfUpdate.checkIntervalHours", "ingest.autoImportPaths")
	// and reconfigure the running tray to reflect it, where that's possible without a
	// restart -- see Runner.Reconfigure.
	SetBool(key string, v bool) error
	SetInt(key string, v int) error
	SetStringSlice(key string, v []string) error

	// SetString is SetBool/SetInt's counterpart for a free-text value --
	// for a caller with no dialog backend (the loopback API's
	// POST /api/settings, and the Wails-bound Settings window). Not every
	// free-text field reaches this: per-integration path rewrites parse
	// into a structured value the generic string path never handles, so
	// they go through SetIntegrationRewrites instead.
	SetString(key, value string) error

	// SetIntegrationPath sets a per-integration catalog path -- a
	// parameterized method rather than one key per integration (lrcat #47,
	// applephotos #46, ...). Same catalogPath/databaseUrl key resolution
	// for every integration, no dialog.
	SetIntegrationPath(id IntegrationID, value string) error

	// SetIntegrationRewrites sets path rewrite rules (from:to pairs) for
	// the given integration. Only meaningful for integrations that have a
	// pathRewrites config field (Resolve).
	SetIntegrationRewrites(id IntegrationID, value string) error

	// SetPathMappings replaces the whole pathMappings list with mappings --
	// the only way to set path mappings; SetString("pathMappings", ...)'s
	// lossy "workstationPath:containerPath, ..." comma/colon string form
	// was retired (issue #236) because it silently corrupted any path
	// containing a comma. An empty (or nil) mappings clears the list.
	SetPathMappings(mappings []PathMappingEntry) error

	// Reload re-reads config.yaml from disk and reconfigures the running
	// tray -- the same path a hand-edit followed by "Reload config" takes,
	// and what every SetBool/SetInt/SetString call does internally after
	// persisting.
	Reload() error

	// OpenConfigFile and RevealConfigFolder shell out to the OS's own
	// "open with default app" / "reveal in file manager" commands --
	// issue #31's minimum bar: an operator can always find and hand-edit
	// config.yaml, even for fields this menu doesn't expose a dialog for.
	//
	// Hand-edit only, on purpose (issue #110 / audit F-14 inventory).
	// The list below enumerates every config field intentionally NOT
	// surfaced by SettingsView, with the reason for each. Fields graduate
	// to SettingsView as their M5/E3 sub-issue lands -- see
	// docs/tray-settings-inventory.md for the per-field tracking table.
	// When a field graduates, remove the matching entry below in the
	// same PR so this comment and the inventory doc never disagree.
	//
	//   * ingest.pollIntervalSecs: low-frequency, restart-only knob;
	//     not worth a menu slot. Operators adjust via OpenConfigFile.
	//   * prune.* (enabled, minAgeHours, intervalMinutes): destructive
	//     subcommand gating; the hand-edit gate is the audit trail.
	//     Toggles here would let a stray click disable a safety check.
	//   * offline.* (queueDbPath, tier0ContainerRoot, drainIntervalSecs):
	//     same shape -- changing the SQLite path or the staging
	//     container root mid-run breaks in-flight drain state, and
	//     drainIntervalSecs is a tuning knob operators rarely touch.
	//   * selfUpdate.repo: a typo in the "owner/name" slug causes the
	//     next update check to fetch from a non-existent or wrong
	//     repo; the hand-edit gate (with a selfupdate log line naming
	//     the resolved repo on every check) is the safety net.
	//   * tray.statusAddr: loopback bind address; a non-loopback
	//     value here would expose the unauthenticated status page on
	//     the network. Restart-only and intentionally hand-edit.
	//   * ingest.exiftoolPath: overrides which exiftool binary the
	//     pooled subprocess manager (internal/exiftool.Pool) invokes;
	//     empty (the default) resolves "exiftool" through PATH. A
	//     rarely-touched operator override, not worth a menu slot --
	//     OpenConfigFile is the path for the (uncommon) machine with
	//     more than one exiftool install.
	OpenConfigFile() error
	RevealConfigFolder() error
}
