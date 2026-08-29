//go:build windows || darwin

package tray

import (
	"fmt"
	"time"

	"fyne.io/systray"
)

// integrationSubmenu owns one catalog integration's own systray items --
// one per Integrations() registry entry, built generically in a loop
// (see newIntegrationsMenu) so adding lrcat (#47)/applephotos (#46) needs
// zero changes to this file. "Sync now" is deliberately NOT dispatched
// from here: it's a Runner action (like "Drain queue now"), wired
// directly into run_supported.go's own select loop via each submenu's
// syncNow item, not a Settings mutation routed through actionCh.
type integrationSubmenu struct {
	id     IntegrationID
	title  string
	parent *systray.MenuItem
	status *systray.MenuItem

	enabled     *systray.MenuItem
	dryRun      *systray.MenuItem
	catalogPath *systray.MenuItem

	intervalParent *systray.MenuItem
	interval15     *systray.MenuItem
	interval60     *systray.MenuItem
	intervalNever  *systray.MenuItem

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

	sub.enabled = parent.AddSubMenuItemCheckbox("Enabled", "Run this integration's sync, on its own timer and via \"Sync now\"", iv.Enabled)
	sub.dryRun = parent.AddSubMenuItemCheckbox("Dry run (log only, emit nothing)", "Resolve and log what a sync would emit without contacting the server", iv.DryRun)
	sub.catalogPath = parent.AddSubMenuItem(catalogPathTitle(iv.CatalogPathSet), "Catalog file this integration reads")

	sub.intervalParent = parent.AddSubMenuItem("Sync every", "How often the tray runs this integration's sync on its own timer")
	sub.interval15 = sub.intervalParent.AddSubMenuItemCheckbox("15 minutes", "", iv.SyncIntervalMinutes == 15)
	sub.interval60 = sub.intervalParent.AddSubMenuItemCheckbox("60 minutes (default)", "", iv.SyncIntervalMinutes == 0 || iv.SyncIntervalMinutes == 60)
	sub.intervalNever = sub.intervalParent.AddSubMenuItemCheckbox("Never (manual only)", "", iv.SyncIntervalMinutes < 0)

	parent.AddSeparator()
	sub.syncNow = parent.AddSubMenuItem("Sync now", "Run one sync pass right now")

	sub.sync(iv, IntegrationStatus{})

	return sub
}

// dispatch translates this submenu's own config-changing clicks into
// menuActions on the shared channel -- a fixed, compile-time select over
// this ONE instance's own static fields, even though newIntegrationsMenu
// runs one such goroutine per registry entry: adding lrcat means one more
// newIntegrationSubmenu call and one more `go sub.dispatch(...)` call, not
// a new select case anywhere.
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

func (sub *integrationSubmenu) dispatch(settings Settings, actionCh chan<- menuAction) {
	id := sub.id
	for {
		select {
		case <-sub.enabled.ClickedCh:
			v := !sub.enabled.Checked()
			sub.send(actionCh, func() error { return settings.SetBool(integrationKey(id, "enabled"), v) })
		case <-sub.dryRun.ClickedCh:
			v := !sub.dryRun.Checked()
			sub.send(actionCh, func() error { return settings.SetBool(integrationKey(id, "dryRun"), v) })
		case <-sub.catalogPath.ClickedCh:
			sub.send(actionCh, func() error { _, err := settings.PromptAndSetIntegrationPath(id); return err })
		case <-sub.interval15.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "syncIntervalMinutes"), 15) })
		case <-sub.interval60.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "syncIntervalMinutes"), 60) })
		case <-sub.intervalNever.ClickedCh:
			sub.send(actionCh, func() error { return settings.SetInt(integrationKey(id, "syncIntervalMinutes"), -1) })
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
	setChecked(sub.enabled, iv.Enabled)
	setChecked(sub.dryRun, iv.DryRun)
	setChecked(sub.interval15, iv.SyncIntervalMinutes == 15)
	setChecked(sub.interval60, iv.SyncIntervalMinutes == 0 || iv.SyncIntervalMinutes == 60)
	setChecked(sub.intervalNever, iv.SyncIntervalMinutes < 0)
	// A Hermes review finding on this PR: a hand-edited config.yaml can
	// set syncIntervalMinutes to a value none of the three menu options
	// represent (e.g. 30) -- all three checkboxes would then show
	// unchecked, silently implying "unset" rather than the real
	// hand-configured value. Rather than guessing which of the three is
	// "closest" (which could misleadingly suggest a click already
	// changed it), the parent item's own title names the actual value so
	// it's never ambiguous.
	if iv.SyncIntervalMinutes != 15 && iv.SyncIntervalMinutes != 0 && iv.SyncIntervalMinutes != 60 && iv.SyncIntervalMinutes >= 0 {
		sub.intervalParent.SetTitle(fmt.Sprintf("Sync every (currently: %d min, hand-configured)", iv.SyncIntervalMinutes))
	} else {
		sub.intervalParent.SetTitle("Sync every")
	}

	sub.catalogPath.SetTitle(catalogPathTitle(iv.CatalogPathSet))
	sub.status.SetTitle(integrationStatusLine(sub.title, iv, status))

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
func integrationStatusLine(title string, iv IntegrationView, status IntegrationStatus) string {
	switch {
	case !iv.Enabled:
		return title + ": disabled"
	case !status.Registered:
		return title + ": enabled, but not fully configured (set the catalog path" + nodeIndexClause(iv) + ")"
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
		return fmt.Sprintf("%s: %s ago%s: %d pair(s), %d emitted, %d skipped%s",
			title, since(ls.At), dryNote, ls.PairsFound, ls.Emitted, ls.Skipped, errNote)
	}
}

// nodeIndexClause extends the "not fully configured" line with a mention
// of the node index specifically when DryRun is false (a live sync needs
// it to resolve either endpoint; a dry run doesn't -- see
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

// integrationsMenu owns the top-level "Node index…" item plus one
// integrationSubmenu per registry entry.
type integrationsMenu struct {
	settings Settings
	actionCh chan<- menuAction

	nodeIndexPath *systray.MenuItem
	nodeIndexErr  error

	subs []*integrationSubmenu
}

// newIntegrationsMenu builds one top-level systray item per
// Integrations() registry entry, plus the shared "Node index…" item --
// called once from onReady, alongside newSettingsMenu. Each integration is
// its own TOP-LEVEL item (not nested under a wrapper "Integrations"
// submenu) specifically to keep every leaf at depth 3
// (root ▸ Luminar Neo ▸ Sync every ▸ 15 minutes), matching the deepest
// tree this repo has actually shipped (Settings ▸ Check every ▸ 1 hour) --
// a wrapper wouldn't fit anything new under it (Enabled/Dry run/Catalog…/
// Sync now are already depth 3) but would push "Sync every"'s own leaves
// to depth 4, untested territory on a menu library this repo has never
// pushed past depth 3.
func newIntegrationsMenu(settings Settings, actionCh chan<- menuAction) *integrationsMenu {
	sv := settings.Snapshot()

	im := &integrationsMenu{settings: settings, actionCh: actionCh}

	im.nodeIndexPath = systray.AddMenuItem(nodeIndexTitle(sv.NodeIndexPathSet), "JSON file mapping workstation paths to nodeUuids -- shared by every catalog integration")

	for _, d := range Integrations() {
		iv, _ := sv.Integration(d.ID)
		im.subs = append(im.subs, newIntegrationSubmenu(d, iv))
	}

	go im.dispatch()
	for _, sub := range im.subs {
		go sub.dispatch(settings, actionCh)
	}

	return im
}

func (im *integrationsMenu) dispatch() {
	for range im.nodeIndexPath.ClickedCh {
		im.send(func() error { _, err := im.settings.PromptAndSet(FieldNodeIndexPath); return err })
	}
}

func (im *integrationsMenu) send(run func() error) {
	select {
	case im.actionCh <- menuAction{run: run, report: im.setLastErr}:
	default:
	}
}

func (im *integrationsMenu) setLastErr(err error) { im.nodeIndexErr = err }

// sync re-renders every item from a fresh Settings snapshot and Runner
// status -- called on every refresh tick and after every action
// completes.
func (im *integrationsMenu) sync(sv SettingsView, st Status) {
	if im.nodeIndexErr != nil {
		im.nodeIndexPath.SetTitle(fmt.Sprintf("%s (last change failed: %v)", nodeIndexTitle(sv.NodeIndexPathSet), im.nodeIndexErr))
	} else {
		im.nodeIndexPath.SetTitle(nodeIndexTitle(sv.NodeIndexPathSet))
	}

	for _, sub := range im.subs {
		iv, _ := sv.Integration(sub.id)
		status, _ := st.Integration(sub.id)
		sub.sync(iv, status)
	}
}

func catalogPathTitle(set bool) string {
	if set {
		return "Catalog… (configured)"
	}
	return "Catalog… (not set)"
}

func nodeIndexTitle(set bool) string {
	if set {
		return "Node index… (configured)"
	}
	return "Node index… (not set)"
}
