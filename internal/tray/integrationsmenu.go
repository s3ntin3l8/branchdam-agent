//go:build windows || darwin

package tray

import (
	"fmt"
	"time"

	"fyne.io/systray"
)

// integrationSubmenu owns one catalog integration's own systray items --
// one per Integrations() registry entry, built generically in a loop (see
// newIntegrationsMenu) so adding lrcat (#47)/applephotos (#46) needs zero
// changes to this file. "Sync now" is deliberately NOT dispatched from
// here: it's a Runner action (like "Drain queue now"), wired directly into
// run_supported.go's own select loop via each submenu's syncNow item, not
// a Settings mutation routed through actionCh.
//
// This menu is deliberately minimal (issue #211's slimming half of Track
// 3d): Enabled, Dry run, Catalog path/database URL, Sync every, and Path
// rewrites all now live in the Wails Settings window's own
// renderIntegrationBlock (cmd/branchdam-agent-ui, Track 3d/#210) instead --
// this submenu keeps only the read-only status line, the Sync timeout
// options (the one config field the window has no equivalent for), and the
// "Sync now" action.
type integrationSubmenu struct {
	id     IntegrationID
	title  string
	parent *systray.MenuItem
	status *systray.MenuItem

	timeoutParent *systray.MenuItem
	timeout30s    *systray.MenuItem // 30 seconds (default)
	timeout1m     *systray.MenuItem // 1 minute
	timeout5m     *systray.MenuItem // 5 minutes
	timeout10m    *systray.MenuItem // 10 minutes

	syncNow *systray.MenuItem

	// lastErr/syncSkipped are read/written ONLY from Run's own select
	// loop (via menuActionResult.report and the syncDoneCh case
	// respectively) -- see settingsMenu.lastErr's own doc comment for why
	// that single-goroutine-ownership discipline is what makes an
	// unsynchronized field safe here.
	lastErr     error
	syncSkipped bool
}

// newIntegrationSubmenu builds one registry entry's own systray items --
// AddMenuItem must already have a menu started (onReady only). iv seeds
// the initial checkbox/title state; sync() is called once at the end so
// the status line and titles are correct before the first refresh tick.
func newIntegrationSubmenu(d IntegrationDescriptor, iv IntegrationView) *integrationSubmenu {
	parent := systray.AddMenuItem(d.Title, d.Title+" catalog sync")
	sub := &integrationSubmenu{id: d.ID, title: d.Title, parent: parent}

	sub.status = parent.AddSubMenuItem("", "Last sync result")
	sub.status.Disable()

	sub.timeoutParent = parent.AddSubMenuItem("Sync timeout", "How long one sync pass can run before timing out")
	sub.timeout30s = sub.timeoutParent.AddSubMenuItemCheckbox("30 seconds (default)", "", iv.TimeoutSecs == 0 || iv.TimeoutSecs == 30)
	sub.timeout1m = sub.timeoutParent.AddSubMenuItemCheckbox("1 minute", "", iv.TimeoutSecs == 60)
	sub.timeout5m = sub.timeoutParent.AddSubMenuItemCheckbox("5 minutes", "", iv.TimeoutSecs == 300)
	sub.timeout10m = sub.timeoutParent.AddSubMenuItemCheckbox("10 minutes", "", iv.TimeoutSecs == 600)

	parent.AddSeparator()
	sub.syncNow = parent.AddSubMenuItem("Sync now", "Run one sync pass right now")

	sub.sync(iv, IntegrationStatus{})

	return sub
}

// integrationKey builds the dotted config.yaml key for one of id's own
// leaves -- "integrations.<id>.<leaf>" -- the one place that string is
// spelled in this package, matching cmd/branchdam-agent's own
// IntegrationBuilder.ConfigKey (which independently derives the identical
// string on the execution side; both packages must agree on the schema
// config.IntegrationsConfig defines, but internal/tray cannot import that
// package's cmd-side registry). Lives here rather than in integrations.go
// (platform-independent, compiles on Linux too) because its only caller is
// this file's own dispatch method -- keeping it here avoids an
// "unused" lint finding on non-windows/darwin builds, where
// integrationsmenu.go itself doesn't compile at all.
func integrationKey(id IntegrationID, leaf string) string {
	return "integrations." + string(id) + "." + leaf
}

// dispatch translates this submenu's own config-changing clicks into
// menuActions on the shared channel -- a fixed, compile-time select over
// this ONE instance's own static fields, even though newIntegrationsMenu
// runs one such goroutine per registry entry: adding lrcat means one more
// newIntegrationSubmenu call and one more `go sub.dispatch(...)` call, not
// a new select case anywhere.
func (sub *integrationSubmenu) dispatch(settings Settings, actionCh chan<- menuAction) {
	id := sub.id
	for {
		select {
		case <-sub.timeout30s.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "timeoutSecs"), 30) })
		case <-sub.timeout1m.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "timeoutSecs"), 60) })
		case <-sub.timeout5m.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "timeoutSecs"), 300) })
		case <-sub.timeout10m.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "timeoutSecs"), 600) })
		}
	}
}

// send is a non-blocking submission to actionCh -- a click while a
// previous action (from ANY menu sharing this channel) is still in flight
// is dropped, matching settingsMenu.send's own precedent, rather than
// queued.
func (sub *integrationSubmenu) send(actionCh chan<- menuAction, run func() error) {
	select {
	case actionCh <- menuAction{run: run, report: sub.setLastErr}:
	default:
	}
}

func (sub *integrationSubmenu) setLastErr(err error) { sub.lastErr = err }

// sync re-renders this submenu from a fresh Settings snapshot entry and
// Runner status entry -- called on every refresh tick and after every
// action completes, exactly mirroring settingsMenu.sync's own contract.
// hasStatus=false (an ID with no Status.Integration entry at all) can only
// happen if Runner and this menu's own Integrations() registry have
// drifted -- rendered as "not yet configured" rather than a crash, since a
// menu item must never panic on a refresh tick.
func (sub *integrationSubmenu) sync(iv IntegrationView, status IntegrationStatus) {
	setChecked(sub.timeout30s, iv.TimeoutSecs == 0 || iv.TimeoutSecs == 30)
	setChecked(sub.timeout1m, iv.TimeoutSecs == 60)
	setChecked(sub.timeout5m, iv.TimeoutSecs == 300)
	setChecked(sub.timeout10m, iv.TimeoutSecs == 600)
	if iv.TimeoutSecs != 0 && iv.TimeoutSecs != 30 && iv.TimeoutSecs != 60 && iv.TimeoutSecs != 300 && iv.TimeoutSecs != 600 {
		sub.timeoutParent.SetTitle(fmt.Sprintf("Sync timeout (currently: %ds, hand-configured)", iv.TimeoutSecs))
	} else {
		sub.timeoutParent.SetTitle("Sync timeout")
	}

	sub.status.SetTitle(integrationStatusLine(sub.id, sub.title, iv, status))

	if sub.lastErr != nil {
		sub.parent.SetTitle(fmt.Sprintf("%s (last change failed: %v)", sub.title, sub.lastErr))
	} else {
		sub.parent.SetTitle(sub.title)
	}

	if sub.syncSkipped {
		sub.syncNow.SetTitle("Sync now (skipped just now -- a sync was already running)")
		sub.syncSkipped = false
	} else {
		sub.syncNow.SetTitle("Sync now")
	}

	if iv.Enabled && status.Registered {
		sub.syncNow.Enable()
	} else {
		sub.syncNow.Disable()
	}
}

// integrationStatusLine builds the human-readable disabled status line --
// the single most important honesty affordance in this menu: "(dry run)"
// must appear on every line where DryRun is true, since Emitted counts
// what a dry-run pass WOULD have posted, not what it actually posted (see
// SyncSummary's own doc comment). An operator must never be able to
// mistake a dry-run count for a real emission.
func integrationStatusLine(id IntegrationID, title string, iv IntegrationView, status IntegrationStatus) string {
	configName := "catalog path"
	if id == IntegrationResolveDB {
		configName = "database URL"
	}
	switch {
	case !iv.Enabled:
		return title + ": disabled"
	case !status.Registered:
		return title + ": enabled, but not fully configured (set the " + configName + nodeIndexClause(iv) + ")"
	case status.LastSync == nil:
		if iv.DryRun {
			return title + ": ready (dry run), no sync yet"
		}
		return title + ": ready, no sync yet"
	case status.LastSync.Err != nil:
		return fmt.Sprintf("%s: last sync %s ago FAILED: %v", title, since(status.LastSync.At), status.LastSync.Err)
	default:
		ls := status.LastSync
		dryNote := ""
		if ls.DryRun {
			dryNote = " (dry run)"
		}
		errNote := ""
		if ls.Errors > 0 {
			errNote = fmt.Sprintf(", %d error(s)", ls.Errors)
		}
		vnNote := ""
		if ls.VirtualNodes > 0 {
			vnNote = fmt.Sprintf(", %d virtual node(s)", ls.VirtualNodes)
		}
		edgeNote := ""
		if ls.EdgesAttached > 0 {
			edgeNote = fmt.Sprintf(", %d edge(s)", ls.EdgesAttached)
		}
		skipLabel := "skipped"
		if iv.ID == IntegrationResolveDB {
			skipLabel = "unresolved"
		}
		return fmt.Sprintf("%s: %s ago%s: %d pair(s), %d emitted, %d %s%s%s%s",
			title, since(ls.At), dryNote, ls.PairsFound, ls.Emitted, ls.Skipped, skipLabel, errNote, vnNote, edgeNote)
	}
}

// nodeIndexClause extends the "not fully configured" line with a mention
// of the node index specifically when DryRun is false (a live sync needs
// it to resolve either endpoint; a dry run does not -- see
// buildIntegrationDeps' own Ready check in cmd/branchdam-agent).
func nodeIndexClause(iv IntegrationView) string {
	if iv.DryRun {
		return ""
	}
	return " and the node index"
}

// since formats how long ago t was, rounded to the second -- matches
// run_supported.go's own summarize() formatting for LastIngest.
func since(t time.Time) string {
	return time.Since(t).Round(time.Second).String()
}

// setChecked is shared by every submenu with a checkbox item whose checked
// state is driven by a Settings snapshot rather than direct user toggling
// feedback -- kept here as its only remaining caller after issue #211
// removed integrationSubmenu's own Enabled/Dry run/Sync-every checkboxes
// (settingsMenu had no other user of it).
func setChecked(item *systray.MenuItem, want bool) {
	switch {
	case want && !item.Checked():
		item.Check()
	case !want && item.Checked():
		item.Uncheck()
	}
}

// integrationsMenu owns one integrationSubmenu per registry entry. The
// top-level "Node index…" item this used to also own is gone (issue #211):
// integrations.nodeIndexPath now lives in the Wails Settings window's
// FREE_TEXT_FIELDS instead.
type integrationsMenu struct {
	subs []*integrationSubmenu
}

// newIntegrationsMenu builds one top-level systray item per
// Integrations() registry entry -- called once from onReady, alongside
// newSettingsMenu. Each integration is its own TOP-LEVEL item (not nested
// under a wrapper "Integrations" submenu) specifically to keep every leaf
// at depth 3 (root ▸ Luminar Neo ▸ Sync timeout ▸ 30 seconds), matching the
// deepest tree this repo has actually shipped (Settings ▸ Check every ▸
// 1 hour).
func newIntegrationsMenu(settings Settings, actionCh chan<- menuAction) *integrationsMenu {
	sv := settings.Snapshot()

	im := &integrationsMenu{}

	for _, d := range Integrations() {
		iv, _ := sv.Integration(d.ID)
		im.subs = append(im.subs, newIntegrationSubmenu(d, iv))
	}

	for _, sub := range im.subs {
		go sub.dispatch(settings, actionCh)
	}

	return im
}

// sync re-renders every item from a fresh Settings snapshot and Runner
// status -- called on every refresh tick and after every action
// completes.
func (im *integrationsMenu) sync(sv SettingsView, st Status) {
	for _, sub := range im.subs {
		iv, _ := sv.Integration(sub.id)
		status, _ := st.Integration(sub.id)
		sub.sync(iv, status)
	}
}
