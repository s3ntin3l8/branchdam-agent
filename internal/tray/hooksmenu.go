//go:build windows || darwin

package tray

import (
	"fyne.io/systray"
)

// hookSubmenu owns one hook's own systray items -- built generically in a
// loop (see newHooksMenu) from HookDescriptors() so a second hook needs
// zero changes to this file. Unlike integrationSubmenu, there is no
// Settings-driven checkbox/prompt here -- a hook has no CatalogSyncConfig
// (see HookID's own doc comment), so every click (Install, Reveal) is a
// Runner action wired directly into run_supported.go's own select loop,
// exactly mirroring integrationSubmenu.syncNow's own "not part of
// dispatch()" precedent.
type hookSubmenu struct {
	id     HookID
	title  string
	parent *systray.MenuItem
	status *systray.MenuItem

	install *systray.MenuItem
	reveal  *systray.MenuItem

	// installSkipped is mutated ONLY from Run's own select loop (via the
	// install-done-channel case) -- see settingsMenu.lastErr's own doc
	// comment for why that single-goroutine-ownership discipline is what
	// makes an unsynchronized field safe here.
	installSkipped bool
}

// newHookSubmenu builds one registry entry's own systray items --
// AddMenuItem must already have a menu started (onReady only). sync() is
// called once at the end so the status line is correct before the first
// refresh tick.
func newHookSubmenu(d HookDescriptor) *hookSubmenu {
	parent := systray.AddMenuItem(d.Title, d.Title+" render-hook installer")
	sub := &hookSubmenu{id: d.ID, title: d.Title, parent: parent}

	sub.status = parent.AddSubMenuItem("", "Installed state, checked once at startup and after each install")
	sub.status.Disable()

	sub.install = parent.AddSubMenuItem("Install / update render hook", "Write (or overwrite) the hook script into the most-writable candidate Scripts/Utility folder")
	sub.reveal = parent.AddSubMenuItem("Reveal Scripts folder", "Open the folder the hook is (or would be) installed into")

	sub.sync(HookStatus{ID: d.ID})

	return sub
}

// sync re-renders this submenu from a fresh Runner status entry -- called
// on every refresh tick and after every install action completes, exactly
// mirroring integrationSubmenu.sync's own contract.
func (sub *hookSubmenu) sync(status HookStatus) {
	sub.status.SetTitle(hookStatusLine(sub.title, status))

	if sub.installSkipped {
		sub.install.SetTitle("Install / update render hook (skipped just now -- already running)")
		sub.installSkipped = false
	} else {
		sub.install.SetTitle("Install / update render hook")
	}
}

// hookStatusLine builds the human-readable disabled status line -- mirrors
// internal/tray/assets/index.html's own DaVinci Resolve section (issue
// #61) wording exactly, so the tray menu and the status page never
// disagree about what a given HookState means.
func hookStatusLine(title string, status HookStatus) string {
	switch {
	case !status.Registered:
		return title + ": not configured"
	case status.State == nil:
		return title + ": not checked yet this session"
	case status.State.Err != nil:
		return title + ": install failed: " + status.State.Err.Error()
	case status.State.Dir == "":
		return title + ": no Scripts/Utility folder found for this platform"
	case !status.State.Installed:
		return title + ": not installed (would install to " + status.State.Dir + ")"
	case status.State.UpToDate:
		return title + ": installed and up to date (checked " + since(status.State.At) + " ago)"
	default:
		return title + ": installed but modified or out of date (checked " + since(status.State.At) + " ago)"
	}
}

// hooksMenu owns one hookSubmenu per HookDescriptors() registry entry.
type hooksMenu struct {
	subs []*hookSubmenu
}

// newHooksMenu builds one top-level systray item per HookDescriptors()
// registry entry -- called once from onReady, alongside
// newIntegrationsMenu/newSettingsMenu. Each hook is its own TOP-LEVEL item,
// a sibling of the Integrations menu's own top-level entries rather than
// nested under it (Integrations() is catalog-sync-only -- see that
// function's own doc comment for why a hook doesn't belong there), for the
// same depth-3 reasoning newIntegrationsMenu's own doc comment gives.
func newHooksMenu() *hooksMenu {
	hm := &hooksMenu{}
	for _, d := range HookDescriptors() {
		hm.subs = append(hm.subs, newHookSubmenu(d))
	}
	return hm
}

// sync re-renders every item from fresh Runner status -- called on every
// refresh tick and after every install action completes.
func (hm *hooksMenu) sync(st Status) {
	for _, sub := range hm.subs {
		status, _ := st.Hook(sub.id)
		sub.sync(status)
	}
}
