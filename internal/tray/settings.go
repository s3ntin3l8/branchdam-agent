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

	ServerBaseURL   string
	ServerAPIKeySet bool

	ArchiveRoot    string
	LocalEditRoot  string
	NamingTemplate string

	// RestartRequired is true once a change to a restart-only field
	// (tray.statusAddr, ingest.cardRoots) has been saved but not yet
	// applied -- see Runner.Reconfigure's doc comment for why those two
	// can't be hot-reloaded. The menu surfaces "Restart now" rather than
	// silently pretending the change already took effect.
	RestartRequired bool
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

	// Reload re-reads config.yaml from disk and reconfigures the running
	// tray -- the same path a hand-edit followed by "Reload config" takes,
	// and what every SetBool/SetInt/PromptAndSet call does internally
	// after persisting.
	Reload() error

	// OpenConfigFile and RevealConfigFolder shell out to the OS's own
	// "open with default app" / "reveal in file manager" commands --
	// issue #31's minimum bar: an operator can always find and hand-edit
	// config.yaml, even for fields this menu doesn't expose a dialog for
	// (pathMappings, ingest.cardRoots).
	OpenConfigFile() error
	RevealConfigFolder() error
}
