//go:build windows || darwin

package tray

import (
	"fmt"

	"fyne.io/systray"
)

// settingsMenu owns the "Settings" submenu's systray items and the
// goroutine translating their clicks into Settings calls. Kept separate
// from run_supported.go's main select loop for the same reason
// ingestNow's worker goroutine is separate: every action here does
// blocking I/O, and each click needs to become one func() error sent to
// actionCh -- non-blocking, dropping a click if one is already in flight
// -- rather than adding a dozen more cases to the already-large select
// loop in Run.
type settingsMenu struct {
	parent   *systray.MenuItem
	settings Settings
	actionCh chan<- menuAction

	// lastErr is set by Run's select loop from settingsDoneCh and
	// rendered into the parent item's title on the next sync -- the only
	// error-reporting surface internal/tray has for a settings action
	// (this package deliberately has no dialog/zenity knowledge; a
	// startup-error-style notification is cmd/branchdam-agent's job, not
	// this one's).
	lastErr error

	startOnLogin         *systray.MenuItem
	selfUpdateEnabled    *systray.MenuItem
	interval1h           *systray.MenuItem
	interval24h          *systray.MenuItem
	intervalNever        *systray.MenuItem
	requireUnbuffered    *systray.MenuItem
	requireDCIM          *systray.MenuItem
	pauseUploadOnMetered *systray.MenuItem
	autoEject            *systray.MenuItem
	serverURL            *systray.MenuItem
	apiKey               *systray.MenuItem
	cardRoots            *systray.MenuItem
	allowedExtensions    *systray.MenuItem
	archiveRoot          *systray.MenuItem
	localEditRoot        *systray.MenuItem
	namingTemplate       *systray.MenuItem
	reloadConfig         *systray.MenuItem
	openConfig           *systray.MenuItem
	revealConfig         *systray.MenuItem
}

// newSettingsMenu builds the "Settings" submenu under the current systray
// menu (systray.AddMenuItem must already have a menu started -- this is
// only ever called from within Run's onReady) and starts the goroutine
// that turns its items' clicks into actions on actionCh.
func newSettingsMenu(settings Settings, actionCh chan<- menuAction) *settingsMenu {
	parent := systray.AddMenuItem("Settings", "Tray and ingest settings")
	sv := settings.Snapshot()

	sm := &settingsMenu{parent: parent, settings: settings, actionCh: actionCh}

	sm.startOnLogin = parent.AddSubMenuItemCheckbox("Start at login", "Register this tray as a per-user login item", sv.StartOnLogin)
	sm.selfUpdateEnabled = parent.AddSubMenuItemCheckbox("Check for updates", "Periodically check GitHub for a newer release (read-only)", sv.SelfUpdateEnabled)

	intervalParent := parent.AddSubMenuItem("Check every", "How often to re-check after the initial startup check")
	sm.interval1h = intervalParent.AddSubMenuItemCheckbox("1 hour", "", sv.SelfUpdateCheckIntervalHrs == 1)
	sm.interval24h = intervalParent.AddSubMenuItemCheckbox("24 hours (default)", "", sv.SelfUpdateCheckIntervalHrs == 0 || sv.SelfUpdateCheckIntervalHrs == 24)
	sm.intervalNever = intervalParent.AddSubMenuItemCheckbox("Never", "", sv.SelfUpdateCheckIntervalHrs < 0)

	sm.requireUnbuffered = parent.AddSubMenuItemCheckbox("Require unbuffered verify", "Fail ingest instead of silently falling back to a buffered re-read", sv.RequireUnbuffered)
	sm.requireDCIM = parent.AddSubMenuItemCheckbox("Require DCIM folder", "Skip volumes that do not contain a DCIM folder", sv.RequireDCIM)
	sm.pauseUploadOnMetered = parent.AddSubMenuItemCheckbox("Pause upload on metered connection", "Pause queue drain and upload streaming when on a metered or hotspot network", sv.PauseUploadOnMetered)
	sm.autoEject = parent.AddSubMenuItemCheckbox("Auto-eject after ingest", "Safely unmount/eject the card volume after verified ingest", sv.AutoEject)

	parent.AddSeparator()

	sm.serverURL = parent.AddSubMenuItem("Server URL…", "branchDAM server URL")
	sm.apiKey = parent.AddSubMenuItem(apiKeyTitle(sv.ServerAPIKeySet), "Agent API key")
	sm.cardRoots = parent.AddSubMenuItem("Watch folders…", "Directories polled for mounted cards")
	sm.allowedExtensions = parent.AddSubMenuItem("Allowed extensions…", "File extensions to ingest (comma-separated, empty for all)")
	sm.archiveRoot = parent.AddSubMenuItem("Archive root…", "Workstation path backing the Tier-3 archive destination")
	sm.localEditRoot = parent.AddSubMenuItem("Local edit root…", "Workstation path for the local edit (scratch) copy")
	sm.namingTemplate = parent.AddSubMenuItem("Naming template…", "Destination path template")

	parent.AddSeparator()

	sm.reloadConfig = parent.AddSubMenuItem("Reload config", "Re-read config.yaml and apply hot-reloadable changes")
	sm.openConfig = parent.AddSubMenuItem("Open config.yaml", "Open the config file in its default editor")
	sm.revealConfig = parent.AddSubMenuItem("Reveal config folder", "Open the folder containing config.yaml")

	go sm.dispatch()

	return sm
}

// send is a non-blocking submission to actionCh -- a click while a
// previous action (from Settings or, now that the channel is shared,
// Integrations) is still in flight is dropped, matching
// run_supported.go's ingestNow precedent, rather than queued.
func (sm *settingsMenu) send(action func() error) {
	select {
	case sm.actionCh <- menuAction{run: action, report: sm.setLastErr}:
	default:
	}
}

// setLastErr is menuAction's report callback for every action this menu
// sends -- called ONLY from Run's own select loop (see menuAction's doc
// comment for why a worker-goroutine call would race with sync() below).
func (sm *settingsMenu) setLastErr(err error) { sm.lastErr = err }

func (sm *settingsMenu) dispatch() {
	for {
		select {
		case <-sm.startOnLogin.ClickedCh:
			v := !sm.startOnLogin.Checked()
			sm.send(func() error { return sm.settings.SetBool("tray.startOnLogin", v) })
		case <-sm.selfUpdateEnabled.ClickedCh:
			v := !sm.selfUpdateEnabled.Checked()
			sm.send(func() error { return sm.settings.SetBool("selfUpdate.enabled", v) })
		case <-sm.interval1h.ClickedCh:
			sm.send(func() error { return sm.settings.SetInt("selfUpdate.checkIntervalHours", 1) })
		case <-sm.interval24h.ClickedCh:
			sm.send(func() error { return sm.settings.SetInt("selfUpdate.checkIntervalHours", 24) })
		case <-sm.intervalNever.ClickedCh:
			sm.send(func() error { return sm.settings.SetInt("selfUpdate.checkIntervalHours", -1) })
		case <-sm.requireUnbuffered.ClickedCh:
			v := !sm.requireUnbuffered.Checked()
			sm.send(func() error { return sm.settings.SetBool("ingest.requireUnbuffered", v) })
		case <-sm.requireDCIM.ClickedCh:
			v := !sm.requireDCIM.Checked()
			sm.send(func() error { return sm.settings.SetBool("ingest.requireDCIM", v) })
		case <-sm.pauseUploadOnMetered.ClickedCh:
			v := !sm.pauseUploadOnMetered.Checked()
			sm.send(func() error { return sm.settings.SetBool("ingest.pauseUploadOnMetered", v) })
		case <-sm.autoEject.ClickedCh:
			v := !sm.autoEject.Checked()
			sm.send(func() error { return sm.settings.SetBool("ingest.autoEject", v) })
		case <-sm.serverURL.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldServerBaseURL); return err })
		case <-sm.apiKey.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldServerAPIKey); return err })
		case <-sm.cardRoots.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldCardRoots); return err })
		case <-sm.allowedExtensions.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldAllowedExtensions); return err })
		case <-sm.archiveRoot.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldArchiveRoot); return err })
		case <-sm.localEditRoot.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldLocalEditRoot); return err })
		case <-sm.namingTemplate.ClickedCh:
			sm.send(func() error { _, err := sm.settings.PromptAndSet(FieldNamingTemplate); return err })
		case <-sm.reloadConfig.ClickedCh:
			sm.send(sm.settings.Reload)
		case <-sm.openConfig.ClickedCh:
			sm.send(sm.settings.OpenConfigFile)
		case <-sm.revealConfig.ClickedCh:
			sm.send(sm.settings.RevealConfigFolder)
		}
	}
}

// sync re-renders every item from a fresh snapshot -- called on every
// refresh tick and after every settings action completes, since a change
// applied through one item (or a hand-edit picked up by Reload) can shift
// what several others should show.
func (sm *settingsMenu) sync(sv SettingsView) {
	setChecked(sm.startOnLogin, sv.StartOnLogin)
	setChecked(sm.selfUpdateEnabled, sv.SelfUpdateEnabled)
	setChecked(sm.interval1h, sv.SelfUpdateCheckIntervalHrs == 1)
	setChecked(sm.interval24h, sv.SelfUpdateCheckIntervalHrs == 0 || sv.SelfUpdateCheckIntervalHrs == 24)
	setChecked(sm.intervalNever, sv.SelfUpdateCheckIntervalHrs < 0)
	setChecked(sm.requireUnbuffered, sv.RequireUnbuffered)
	setChecked(sm.requireDCIM, sv.RequireDCIM)
	setChecked(sm.pauseUploadOnMetered, sv.PauseUploadOnMetered)
	setChecked(sm.autoEject, sv.AutoEject)

	sm.apiKey.SetTitle(apiKeyTitle(sv.ServerAPIKeySet))

	if sm.lastErr != nil {
		sm.parent.SetTitle(fmt.Sprintf("Settings (last change failed: %v)", sm.lastErr))
	} else {
		sm.parent.SetTitle("Settings")
	}
}

func setChecked(item *systray.MenuItem, want bool) {
	switch {
	case want && !item.Checked():
		item.Check()
	case !want && item.Checked():
		item.Uncheck()
	}
}

func apiKeyTitle(set bool) string {
	if set {
		return "API key… (configured)"
	}
	return "API key… (not set)"
}
