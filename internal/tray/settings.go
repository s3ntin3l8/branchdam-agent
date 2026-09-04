package tray

// SettingsField identifies one free-text configuration field the settings
// menu edits via an external dialog (cmd/branchdam-agent's zenity-backed
// prompt) rather than a native checkbox/submenu -- server URL, API key,
// the two ingest roots, and the naming template. A single enum plus one
// Settings.PromptAndSet method, rather than five near-identical
// PromptServerURL/PromptAPIKey/... methods.
type SettingsField int

const (
	FieldServerBaseURL SettingsField = iota
	FieldServerAPIKey
	FieldArchiveRoot
	FieldLocalEditRoot
	FieldNamingTemplate
	// FieldNodeIndexPath is appended, not inserted -- these are iota
	// constants, so inserting in the middle would silently renumber the
	// five above. It's a single field (unlike per-integration catalog
	// paths, which go through PromptAndSetIntegrationPath instead)
	// because the node index is shared by every catalog-reading
	// integration -- see config.IntegrationsConfig.NodeIndexPath's own
	// doc comment.
	FieldNodeIndexPath
	FieldCardRoots
	FieldAllowedExtensions
)

// SettingsView is a read-only snapshot of the on-disk configuration
// fields the settings menu renders. Booleans/enums render as native
// checkboxes/submenus directly from these fields; the five free-text
// fields (server URL, API key, the two ingest roots, naming template) are
// edited via Settings.PromptAndSet instead -- ServerAPIKeySet is
// deliberately a bool, never the key itself, so the menu can say
// "configured" without ever holding the secret in memory it doesn't
// already need for its own purposes.
type SettingsView struct {
	ConfigPath string

	StartOnLogin bool

	SelfUpdateEnabled bool
	// SelfUpdateCheckIntervalHrs mirrors config.SelfUpdateConfig.CheckIntervalHours
	// verbatim: 0 means unset (defaults to 24 elsewhere), negative means
	// "never re-check after the initial one."
	SelfUpdateCheckIntervalHrs int

	RequireUnbuffered bool
	RequireDCIM       bool

	ServerBaseURL   string
	ServerAPIKeySet bool

	ArchiveRoot       string
	LocalEditRoot     string
	NamingTemplate    string
	AllowedExtensions []string

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
// into the file picker via PromptAndSetIntegrationPath's own
// defaultValue-style lookup, not read from here.
type IntegrationView struct {
	ID             IntegrationID
	Enabled        bool
	DryRun         bool
	CatalogPath    string
	CatalogPathSet bool
	// SyncIntervalMinutes mirrors config.CatalogSyncConfig.SyncIntervalMinutes
	// verbatim: 0 means unset (defaults to 60 elsewhere), negative means
	// "manual only" -- same convention as SettingsView.SelfUpdateCheckIntervalHrs.
	SyncIntervalMinutes int
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

	// SetBool/SetInt persist one dotted config key (e.g.
	// "tray.startOnLogin", "selfUpdate.checkIntervalHours") and reconfigure
	// the running tray to reflect it, where that's possible without a
	// restart -- see Runner.Reconfigure.
	SetBool(key string, v bool) error
	SetInt(key string, v int) error

	// PromptAndSet shows whatever dialog backend the implementation uses
	// for field, applies the answer if one was given, and reconfigures the
	// running tray. ok is false when the operator dismissed the dialog --
	// distinct from err, which means the dialog itself failed to render or
	// the change failed to save.
	PromptAndSet(field SettingsField) (ok bool, err error)

	// PromptAndSetIntegrationPath is PromptAndSet's counterpart for a
	// per-integration catalog path -- a parameterized method rather than
	// one SettingsField enum value per integration (lrcat #47, applephotos
	// #46, ...), matching SettingsField's own design note: a single enum
	// plus one method beats N near-identical ones. Same ok/err contract as
	// PromptAndSet.
	PromptAndSetIntegrationPath(id IntegrationID) (ok bool, err error)

	// Reload re-reads config.yaml from disk and reconfigures the running
	// tray -- the same path a hand-edit followed by "Reload config" takes,
	// and what every SetBool/SetInt/PromptAndSet call does internally
	// after persisting.
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
	//   * pathMappings: hand-edit by design (config.go:446-456 -- each
	//     rule is a workstation-to-container prefix pair, not a single
	//     value, and a wrong entry silently misroutes every event for
	//     a prefix). OpenConfigFile/Pre-flight stay the operator path.
	//   * ingest.cardRoots: pending M5 #78 (graduates once a tray-side
	//     Detector restart lands; see the "cardRoots" line in
	//     docs/tray-settings-inventory.md).
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
	//
	// Fields the issue (#110) lists as future graduates but that do
	// not yet exist in the Config struct (no M5 sub-issue has landed):
	// ingest.autoEject (#87), ingest.requireDCIM (#81),
	// ingest.pauseUploadOnMetered (#84), ingest.autoImportPaths (#79),
	// tray.confirmDestructive (E3 #S2-14). Each appears in
	// docs/tray-settings-inventory.md's table as a not-yet-applicable
	// row -- it is a settings.go follow-up, not a pre-existing gap.
	OpenConfigFile() error
	RevealConfigFolder() error
}
