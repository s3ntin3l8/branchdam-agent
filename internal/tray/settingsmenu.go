//go:build windows || darwin

package tray

import (
	"fmt"

	"fyne.io/systray"
)

// settingsMenu owns the "Advanced" submenu's systray items and the
// goroutine translating their clicks into Settings calls. Kept separate
// from run_supported.go's main select loop for the same reason
// ingestNow's worker goroutine is separate: every action here does
// blocking I/O, and each click needs to become one func() error sent to
// actionCh -- non-blocking, dropping a click if one is already in flight
// -- rather than adding more cases to the already-large select loop in Run.
//
// This menu is deliberately minimal (issue #211's slimming half of Track
// 3d, renamed from "Settings" to "Advanced" in the tray/window UX rethink
// since it no longer leads to any actual setting): every free-text/checkbox
// settings field it used to own (Server URL, API key, Agent ID, watch
// folders, allowed extensions, archive/local edit root, path mappings,
// naming template, start-on-login, confirm destructive, self-update
// enabled/interval, require-unbuffered, require-DCIM, pause-on-metered,
// auto-eject) now lives in the Wails Settings window (cmd/branchdam-agent-ui,
// Track 3d/#210) instead. What remains here has no window equivalent:
// Reload/OpenConfigFile/RevealConfigFolder are hand-edit-config affordances,
// not settings values, and the window has no "reveal this file in
// Finder/Explorer" button. See docs/tray-settings-inventory.md for the full
// graduation table.
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

	reloadConfig *systray.MenuItem
	openConfig   *systray.MenuItem
	revealConfig *systray.MenuItem
}

// newSettingsMenu builds the "Advanced" submenu under the current systray
// menu (systray.AddMenuItem must already have a menu started -- this is
// only ever called from within Run's onReady) and starts the goroutine
// that turns its items' clicks into actions on actionCh.
func newSettingsMenu(settings Settings, actionCh chan<- menuAction) *settingsMenu {
	parent := systray.AddMenuItem("Advanced", "Config file actions -- all settings live in the branchDAM window")

	sm := &settingsMenu{parent: parent, settings: settings, actionCh: actionCh}

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
		case <-sm.reloadConfig.ClickedCh:
			sm.send(sm.settings.Reload)
		case <-sm.openConfig.ClickedCh:
			sm.send(sm.settings.OpenConfigFile)
		case <-sm.revealConfig.ClickedCh:
			sm.send(sm.settings.RevealConfigFolder)
		}
	}
}

// sync re-renders the parent item's title from the last action's outcome --
// called on every refresh tick and after every settings action completes.
func (sm *settingsMenu) sync(_ SettingsView) {
	if sm.lastErr != nil {
		sm.parent.SetTitle(fmt.Sprintf("Advanced (last change failed: %v)", sm.lastErr))
	} else {
		sm.parent.SetTitle("Advanced")
	}
}
