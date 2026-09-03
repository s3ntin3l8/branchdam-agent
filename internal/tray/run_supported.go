//go:build windows || darwin

// This file holds the repo's only fyne.io/systray import, gated to the two
// platforms the tray is meant to run on (per the plan doc's UI-stack
// section and issue #3's scope: "tray-resident on Windows 11 and macOS
// Apple Silicon"). Build-tagging it out on every other GOOS keeps `go
// build`/`go vet`/`go test ./...` on the Linux CI runner (the repo's
// required test-go/lint-and-test check) from ever needing to resolve a
// systray backend that isn't meant to run there -- see run_unsupported.go
// for the Linux/other stub. Verified during development: fyne.io/systray
// v1.12.2 itself cross-compiles for GOOS=windows with CGO_ENABLED=0 (pure
// Go, syscall-based) but needs cgo for GOOS=darwin, so this file compiles
// cleanly when cross-built for windows from Linux CI and requires either a
// real macOS host or a darwin cgo cross-toolchain for darwin -- see this
// PR's body for what was actually verified in CI.
package tray

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"fyne.io/systray"
)

// menuRefreshInterval is how often the informational (disabled) menu items
// re-render from Runner.Status -- covers both a card the watch loop just
// ingested and a self-update note that changed since the tray started.
const menuRefreshInterval = 5 * time.Second

// drainPruneClickTimeout bounds a menu-triggered "Drain queue now"/"Prune
// now" pass -- the tray's own background timer path
// (cmd/branchdam-agent/queueagent.go's periodicPassTimeout) already bounds
// every automatic pass to the same duration; a manual click went unbounded
// until a Hermes review finding on this PR caught the inconsistency.
const drainPruneClickTimeout = 2 * time.Minute

// integrationSyncClickTimeout bounds a menu-triggered "Sync now" pass --
// deliberately NOT drainPruneClickTimeout (2 minutes): a large third-party
// catalog plus a per-edge POST loop can run considerably longer than a
// queue drain/prune pass. Matches cmd/branchdam-agent's own
// integrationSyncTimeout, which bounds the SAME operation on its
// timer-driven path.
const integrationSyncClickTimeout = 10 * time.Minute

// hookInstallClickTimeout bounds a menu-triggered "Install / update render
// hook" pass -- matches drainPruneClickTimeout's own 2 minutes, not
// integrationSyncClickTimeout's 10: an atomic temp-then-rename script
// write is a small, local (or at worst LAN-networked scriptsDir) file
// operation, much closer in shape to a drain/prune pass than a
// third-party-catalog sync.
const hookInstallClickTimeout = 2 * time.Minute

// syncClickResult is what each integrationSubmenu's own "Sync now" worker
// goroutine feeds back to the main select loop.
type syncClickResult struct {
	id  IntegrationID
	ran bool
}

// hookInstallResult is what each hookSubmenu's own "Install / update
// render hook" worker goroutine feeds back to the main select loop --
// mirrors syncClickResult's own shape exactly.
type hookInstallResult struct {
	id  HookID
	ran bool
}

// hookRevealResult is what each hookSubmenu's own "Reveal Scripts folder"
// worker goroutine feeds back to the main select loop -- err is nil on a
// successful shell-out, matching Runner.RevealHook's own contract.
type hookRevealResult struct {
	id  HookID
	err error
}

// applyResult is what the install goroutine feeds back to the select loop.
type applyResult struct {
	version string
	err     error
}

// menuAction pairs one blocking Settings call with the menu item that owns
// its error -- settingsMenu and integrationsMenu (and each of the latter's
// per-registry-entry integrationSubmenus) all share ONE worker
// goroutine/channel (see onReady below), so a plain func() error is no
// longer enough to route a result back to the RIGHT lastErr field. The
// worker goroutine must never call report itself: report mutates a
// *systray.MenuItem-owning struct's own field, which sync() (called from
// Run's own select loop) also reads -- calling report from any other
// goroutine would race. It is therefore called ONLY from the select loop's
// menuDoneCh case, never from the worker.
type menuAction struct {
	run    func() error
	report func(error)
}

// menuActionResult is what the shared worker goroutine feeds back --
// report is carried through unchanged so the select loop can invoke it
// without needing to know which menu (or which of N integration
// submenus) originated the action.
type menuActionResult struct {
	report func(error)
	err    error
}

// Run starts the tray application on supported platforms (windows/darwin).
//
// Blocks until the user quits or a fatal error occurs (e.g. self-update
// failure or unexpected detector crash). Returns Outcome describing whether
// a self-update requested a restart, so main() can exit with the right code.
//
// r drives the tray's state snapshot and ingest triggers; statusURL is shown
// in the menu and opened by "Open status page". up drives the "Install and
// restart" affordance; settings drives the "Settings" submenu (issue #31) --
// Run itself does not know how to check for updates or persist config,
// matching Runner's own separation from the ingest core (see tray.go).
//
// confirm and confirmDestructive gate the four destructive menu actions
// (issue #108 / E3 #S2-14: "Drain queue now", "Prune now", "Install
// and restart", "Roll back"). When confirmDestructive is true, every
// one of those four click handlers calls confirm(title, body) before
// dispatching the work, and skips the action on a Cancel/false answer.
// When false, the prompt is skipped entirely -- power users who want
// fire-and-forget clicks set tray.confirmDestructive: false in
// config. confirm itself is a function the production wiring supplies
// (a re-exec of `dialog -kind question ...`); Run never imports a
// dialog backend directly.
//
// pickDir and notify are the OS dialog callbacks for "Import from folder…"
// and tray notifications (issue #80).
func Run(
	ctx context.Context,
	r *Runner,
	statusURL string,
	up SelfUpdater,
	settings Settings,
	confirm func(ctx context.Context, title, body string) bool,
	confirmDestructive bool,
	pickDir func(ctx context.Context, title string) (string, error),
	notify func(ctx context.Context, title, message string),
) (Outcome, error) {
	errCh := make(chan error, 1)
	var outcome Outcome

	onReady := func() {
		systray.SetIcon(buildTrayIcon())
		systray.SetTitle("branchDAM")
		systray.SetTooltip("branchDAM agent")

		statusItem := systray.AddMenuItem("Status: starting...", "Current tray status")
		statusItem.Disable()
		openStatus := systray.AddMenuItem("Open status page", statusURL)
		systray.AddSeparator()

		updateItem := systray.AddMenuItem("Self-update: checking...", "Self-update status")
		updateItem.Disable()
		installItem := systray.AddMenuItem("Install and restart", "Download and apply the latest release, then restart")
		installItem.Hide()
		rollbackItem := systray.AddMenuItem("Roll back", "Restore the previously applied version")
		rollbackItem.Hide()
		systray.AddSeparator()

		watchItem := systray.AddMenuItem("Watch directories: none configured", "Directories polled for inserted cards")
		watchItem.Disable()
		ingestNow := systray.AddMenuItem("Ingest now", "Run one ingest pass over every configured watch directory")
		pauseItem := systray.AddMenuItemCheckbox("⏸ Pause ingest", "Temporarily suspend automatic card detection and queue draining", false)
		importFolder := systray.AddMenuItem("Import from folder…", "Ingest files from a selected folder")
		systray.AddSeparator()

		queueItem := systray.AddMenuItem("Queue: not configured", "Offline queue backlog (offline.queueDbPath)")
		queueItem.Disable()
		drainNow := systray.AddMenuItem("Drain queue now", "Submit pending events, copy pending archive bytes, and rebase eligible rows")
		pruneNow := systray.AddMenuItem("Prune now", "Delete verified local-edit-root mirrors eligible for cleanup")
		systray.AddSeparator()

		// Every config-changing action -- a Settings checkbox/free-text
		// prompt/Reload/Open/Reveal, AND (now) an Integrations
		// checkbox/catalog-path prompt/interval choice -- runs through
		// this ONE shared worker, for the same reason ingestNow does:
		// they all do blocking I/O (disk, a dialog subprocess, rebuilding
		// the ingest Engine and integration syncers), and running any of
		// that inline in the select loop below would freeze the whole
		// menu, including Quit, for as long as it takes. Each menu's own
		// dispatch goroutine feeds this channel directly (non-blocking,
		// drops a click if one is already in flight anywhere -- Settings
		// and Integrations included) rather than routing through the
		// select loop below. See menuAction's own doc comment for why the
		// worker never calls report itself.
		menuActionCh := make(chan menuAction, 1)
		menuDoneCh := make(chan menuActionResult, 1)
		go func() {
			for a := range menuActionCh {
				menuDoneCh <- menuActionResult{report: a.report, err: a.run()}
			}
		}()

		im := newIntegrationsMenu(settings, menuActionCh)
		systray.AddSeparator()

		// The hooks menu (issue #68) takes no Settings/actionCh dependency
		// at all -- a hook has no menu-editable config in this PR's scope,
		// see hookSubmenu's own doc comment -- so, unlike im/sm, it's built
		// with no arguments.
		hm := newHooksMenu()
		systray.AddSeparator()

		sm := newSettingsMenu(settings, menuActionCh)
		restartNowItem := sm.parent.AddSubMenuItem("Restart now", "Apply a change that needs a restart (status address)")
		restartNowItem.Hide()
		systray.AddSeparator()

		quitItem := systray.AddMenuItem("Quit", "Stop the branchDAM agent tray")

		var applying, rollingBack bool
		// drainSkipped/pruneSkipped are set by the drainDoneCh/pruneDoneCh
		// handlers below when a menu click's TriggerDrain/TriggerPrune call
		// reported ran=false (a pass was already running, or -- for prune
		// -- an ingest holds Runner.gate) -- otherwise a dropped click is
		// silent and looks identical to a successful no-op pass. refresh()
		// shows the note for exactly one tick, then clears it.
		var drainSkipped, pruneSkipped bool

		refresh := func() {
			us := up.Status()
			st := r.Status(us)
			systray.SetTooltip(FormatTooltip(st))
			statusItem.SetTitle("Status: " + summarize(st))
			updateItem.SetTitle("Self-update: " + us.Note())

			if st.Paused {
				pauseItem.Check()
				pauseItem.SetTitle("▶ Resume ingest")
				pauseItem.SetTooltip("Resume automatic card detection and queue draining")
				systray.SetIcon(buildPausedTrayIcon())
				systray.SetTooltip("branchDAM agent (ingest paused)")
			} else {
				pauseItem.Uncheck()
				pauseItem.SetTitle("⏸ Pause ingest")
				pauseItem.SetTooltip("Temporarily suspend automatic card detection and queue draining")
				systray.SetIcon(buildTrayIcon())
				systray.SetTooltip("branchDAM agent")
			}

			// Watch dirs can change out from under this menu now that
			// Reconfigure exists (issue #31's settings menu) -- re-render
			// on every tick rather than only at startup.
			if len(st.WatchDirs) == 0 {
				watchItem.SetTitle("Watch directories: none configured")
				ingestNow.Disable()
			} else {
				watchItem.SetTitle(fmt.Sprintf("Watching %d director%s", len(st.WatchDirs), plural(len(st.WatchDirs))))
				ingestNow.Enable()
			}

			qs := st.QueueStatus
			switch {
			case !qs.Configured:
				queueItem.SetTitle("Queue: not configured")
				drainNow.Disable()
			case qs.Err != nil:
				queueItem.SetTitle(fmt.Sprintf("Queue: error (%v)", qs.Err))
				drainNow.Enable()
			default:
				queueItem.SetTitle(fmt.Sprintf("Queue: %d pending, %d failed", qs.Counts.Pending(), qs.Counts.Failed))
				drainNow.Enable()
			}
			if qs.Configured && qs.PruneEnabled {
				pruneNow.Enable()
			} else {
				pruneNow.Disable()
			}

			if drainSkipped {
				drainNow.SetTitle("Drain queue now (skipped just now -- a pass was already running)")
				drainSkipped = false
			} else {
				drainNow.SetTitle("Drain queue now")
			}
			if pruneSkipped {
				pruneNow.SetTitle("Prune now (skipped just now -- already running, or an ingest is in progress)")
				pruneSkipped = false
			} else {
				pruneNow.SetTitle("Prune now")
			}

			sv := settings.Snapshot()
			sm.sync(sv)
			im.sync(sv, st)
			hm.sync(st)
			if sv.RestartRequired {
				restartNowItem.Show()
			} else {
				restartNowItem.Hide()
			}

			switch {
			case applying:
				// Left as "Installing..." by the click handler; don't
				// stomp it with a stale UpdateFound-driven title.
			case us.UpdateFound:
				installItem.Show()
				if st.Busy || rollingBack {
					installItem.SetTitle(fmt.Sprintf("Install and restart (waiting for ingest of %s to finish)", st.BusyCard))
					installItem.Disable()
				} else {
					installItem.SetTitle("Install and restart")
					installItem.Enable()
				}
			default:
				installItem.Hide()
			}

			switch {
			case rollingBack:
				// Left as "Rolling back..." by the click handler; don't
				// stomp it with a stale RollbackAvailable-driven title.
			default:
				if rbVersion, ok := up.RollbackAvailable(); ok {
					rollbackItem.Show()
					if st.Busy || applying {
						rollbackItem.SetTitle(fmt.Sprintf("Roll back to %s (waiting for ingest of %s to finish)", rbVersion, st.BusyCard))
						rollbackItem.Disable()
					} else {
						rollbackItem.SetTitle(fmt.Sprintf("Roll back to %s", rbVersion))
						rollbackItem.Enable()
					}
				} else {
					rollbackItem.Hide()
				}
			}
		}
		refresh()

		ticker := time.NewTicker(menuRefreshInterval)
		defer ticker.Stop()

		r.SetOnPauseChange(func(paused bool) {
			refresh()
		})
		r.SetOnCardIngested(refresh)
		r.SetDetectorErrorHandler(func(err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				select {
				case errCh <- err:
				default:
				}
			}
		})
		if len(r.WatchDirs()) > 0 {
			r.ReconfigureDetector(ctx, r.WatchDirs())
		}

		// ingestNow's click used to call r.TriggerIngest inline in this
		// select loop. Now that TriggerIngest blocks on Runner.gate for
		// its whole duration (see tray.go), doing that here would freeze
		// the entire menu -- including Quit -- for as long as a
		// concurrent card-detector ingest takes. Run it in a goroutine
		// and report back via ingestDoneCh instead.
		ingestDoneCh := make(chan struct{}, 1)
		ingestRequestCh := make(chan struct{}, 1)
		go func() {
			for range ingestRequestCh {
				for _, dir := range r.WatchDirs() {
					r.TriggerIngest(ctx, dir)
				}
				ingestDoneCh <- struct{}{}
			}
		}()

		// "Import from folder…" manual source selection (issue #80): opens
		// an OS directory picker dialog, checks whether the chosen directory
		// is already being watched/ingested (showing an OS notification if so),
		// and passes it to TriggerIngest directly. Running it in a worker
		// goroutine avoids freezing the GUI loop while the picker is open or
		// while ingest is running.
		importFolderDoneCh := make(chan struct{}, 1)
		importFolderRequestCh := make(chan struct{}, 1)
		go func() {
			for range importFolderRequestCh {
				handleImportFolder(ctx, r, func(pctx context.Context) (string, error) {
					if pickDir == nil {
						return "", errors.New("tray: no directory picker configured")
					}
					return pickDir(pctx, "Import from folder…")
				}, func(nctx context.Context, msg string) {
					if notify != nil {
						notify(nctx, "branchDAM Agent", msg)
					}
				})
				importFolderDoneCh <- struct{}{}
			}
		}()

		// "Drain queue now" and "Prune now" follow the exact same
		// non-blocking-request / worker-goroutine shape as ingestNow above,
		// for the same reason: TriggerDrain/TriggerPrune do blocking I/O
		// (network, disk), and running either inline in the select loop
		// would freeze the whole menu, including Quit, for as long as a
		// pass takes. This is on top of, not instead of, the tray's own
		// background timers (cmd/branchdam-agent/tray.go) that call the
		// same Runner methods on a schedule -- a manual click and a timer
		// tick share TriggerDrain/TriggerPrune's own locking (drainMu /
		// Runner.gate via TryLockIdle), so the two can never race each
		// other into a double pass.
		drainDoneCh := make(chan bool, 1)
		drainRequestCh := make(chan struct{}, 1)
		go func() {
			for range drainRequestCh {
				dctx, cancel := context.WithTimeout(ctx, drainPruneClickTimeout)
				_, ran := r.TriggerDrain(dctx)
				cancel()
				drainDoneCh <- ran
			}
		}()

		pruneDoneCh := make(chan bool, 1)
		pruneRequestCh := make(chan struct{}, 1)
		go func() {
			for range pruneRequestCh {
				pctx, cancel := context.WithTimeout(ctx, drainPruneClickTimeout)
				_, ran := r.TriggerPrune(pctx)
				cancel()
				pruneDoneCh <- ran
			}
		}()

		// "Sync now" per integration, one worker goroutine per registry
		// entry (built generically from im.subs, so a new integration
		// needs no new code here) -- each goroutine reads its OWN
		// syncNow.ClickedCh directly in a for-range loop rather than a
		// select case, sidestepping the "N dynamic cases in one static
		// select" problem entirely. TriggerSync's own per-ID in-flight
		// tracking (tray.go) already makes a rapid double-click safe even
		// without a request-channel dedup layer like ingestRequestCh's:
		// a for-range loop over one item's own ClickedCh already
		// serializes calls for that ID, and a second click arriving while
		// the first is still running simply waits its turn in the
		// channel's own buffer -- TriggerSync would report ran=false for
		// it regardless, since only one call per ID and this goroutine
		// can be in flight at a time. Only the RESULT needs to reach the
		// main select loop (to mutate the right submenu's own fields,
		// which must only ever happen from one goroutine, matching every
		// other *systray.MenuItem mutation in this file).
		syncDoneCh := make(chan syncClickResult, 1)
		for _, sub := range im.subs {
			sub := sub
			go func() {
				for range sub.syncNow.ClickedCh {
					sctx, cancel := context.WithTimeout(ctx, integrationSyncClickTimeout)
					_, ran := r.TriggerSync(sctx, sub.id)
					cancel()
					syncDoneCh <- syncClickResult{id: sub.id, ran: ran}
				}
			}()
		}

		// "Install / update render hook" per hook, same shape as "Sync
		// now" above: one worker goroutine per registry entry, reading its
		// own install.ClickedCh directly rather than a select case, so a
		// second hook needs no new code here. TriggerHookInstall's own
		// per-ID in-flight tracking (tray.go) makes a rapid double-click
		// safe the same way TriggerSync's does.
		//
		// Buffered to len(hm.subs), not 1: a Hermes review finding on this
		// PR -- with a cap-1 channel, N hooks completing their installs in
		// the same instant would have all but one worker block on its send
		// until the select loop drains the channel, delaying that worker's
		// NEXT read of its own install.ClickedCh. One hook today makes this
		// unreachable, but sizing it correctly now is what actually makes
		// "a second hook needs no new code here" true.
		hookInstallDoneCh := make(chan hookInstallResult, len(hm.subs))
		for _, sub := range hm.subs {
			sub := sub
			go func() {
				for range sub.install.ClickedCh {
					hctx, cancel := context.WithTimeout(ctx, hookInstallClickTimeout)
					_, ran := r.TriggerHookInstall(hctx, sub.id)
					cancel()
					hookInstallDoneCh <- hookInstallResult{id: sub.id, ran: ran}
				}
			}()
		}

		// "Reveal Scripts folder" -- a Hermes review finding on this PR:
		// the original version discarded RevealHook's error entirely
		// (matching openBrowser's own precedent for "Open status page"),
		// which meant a failed Reveal -- an unregistered hook, or the
		// installer's own shell-out failing -- had zero feedback anywhere,
		// unlike Install (which has both the skipped-note and the status
		// line). Reveal never mutates HookState, so it still needs no
		// SetHookState/hookInFlight involvement and (unlike Install) the
		// result never triggers a refresh of the status line -- only the
		// parent item's own title, via revealErr.
		hookRevealDoneCh := make(chan hookRevealResult, len(hm.subs))
		for _, sub := range hm.subs {
			sub := sub
			go func() {
				for range sub.reveal.ClickedCh {
					hookRevealDoneCh <- hookRevealResult{id: sub.id, err: r.RevealHook(sub.id)}
				}
			}()
		}

		applyDoneCh := make(chan applyResult, 1)
		rollbackDoneCh := make(chan applyResult, 1)
		var releaseGate func()
		// quitRequested defers an in-flight ctx.Done()/Quit-click until
		// the current apply resolves, rather than abandoning it.
		// ApplyLatest's goroutine deliberately runs on a context
		// decoupled from ctx (context.WithoutCancel) so a signal-derived
		// shutdown can't interrupt a Windows sibling-then-primary swap
		// mid-way -- but that guarantee is worthless if this select loop
		// quits and the whole process exits out from under that goroutine
		// regardless. Quitting is deferred, not ignored: applyDoneCh's
		// own case still quits once the apply (bounded by its own
		// 10-minute timeout) actually finishes.
		var quitRequested bool

		for {
			select {
			case <-ctx.Done():
				if applying || rollingBack {
					quitRequested = true
					continue
				}
				systray.Quit()
				return
			case <-quitItem.ClickedCh:
				if applying || rollingBack {
					quitRequested = true
					continue
				}
				systray.Quit()
				return
			case <-openStatus.ClickedCh:
				_ = openBrowser(statusURL)
			case <-pauseItem.ClickedCh:
				r.SetPaused(!r.Paused())
			case <-ingestNow.ClickedCh:
				select {
				case ingestRequestCh <- struct{}{}:
				default:
					// an ingest request is already queued/running; drop
					// this click rather than pile up requests.
				}
			case <-ingestDoneCh:
				refresh()
			case <-importFolder.ClickedCh:
				select {
				case importFolderRequestCh <- struct{}{}:
				default:
					// a folder import is already queued/running; drop
					// this click rather than pile up requests.
				}
			case <-importFolderDoneCh:
				refresh()
			case <-drainNow.ClickedCh:
				// Confirmation gate (issue #108 / E3 #S2-14): a drain
				// pass POSTs all pending node_created events to the
				// server and rebases eligible rows -- destructive in
				// the sense that re-running it on a queue the operator
				// didn't realize was ready would surprise them. The
				// default in title/body matches the issue's exact
				// wording.
				if !confirmDestructiveAction(ctx, confirm, confirmDestructive,
					"Confirm drain queue",
					"Drain the offline queue now? This will POST all pending node_created events to branchDAM. Cancel to defer.") {
					continue
				}
				select {
				case drainRequestCh <- struct{}{}:
				default:
					// a drain pass is already queued/running; drop this
					// click rather than pile up requests.
				}
			case ran := <-drainDoneCh:
				if !ran {
					drainSkipped = true
				}
				refresh()
			case <-pruneNow.ClickedCh:
				// Confirmation gate (issue #108 / E3 #S2-14): prune
				// DELETES from ingest.LocalEditRoot. A double-click
				// against the wrong mount is the canonical "silent
				// data loss" the issue was filed to fix -- AGENTS.md
				// invariant #9 (prune safety) names this exact
				// scenario.
				if !confirmDestructiveAction(ctx, confirm, confirmDestructive,
					"Confirm prune",
					"Delete verified local files matching LocalEditRoot? This is destructive and cannot be undone. Cancel to keep them.") {
					continue
				}
				select {
				case pruneRequestCh <- struct{}{}:
				default:
				}
			case ran := <-pruneDoneCh:
				if !ran {
					pruneSkipped = true
				}
				refresh()
			case res := <-menuDoneCh:
				res.report(res.err)
				refresh()
			case res := <-syncDoneCh:
				if !res.ran {
					for _, sub := range im.subs {
						if sub.id == res.id {
							sub.syncSkipped = true
						}
					}
				}
				refresh()
			case res := <-hookInstallDoneCh:
				if !res.ran {
					for _, sub := range hm.subs {
						if sub.id == res.id {
							sub.installSkipped = true
						}
					}
				}
				refresh()
			case res := <-hookRevealDoneCh:
				for _, sub := range hm.subs {
					if sub.id == res.id {
						sub.revealErr = res.err
					}
				}
				refresh()
			case <-restartNowItem.ClickedCh:
				if rel, ok := r.TryLockIdle(); ok {
					// Belt-and-suspenders release, matching the
					// self-update success path below: the process exits
					// right after this select loop returns, so
					// Runner.gate is abandoned either way, but calling
					// this explicitly means that's never load-bearing.
					rel()
					outcome = Outcome{RestartRequested: true}
					systray.Quit()
					return
				}
				refresh() // still busy -- leave "Restart now" showing, try again later
			case <-installItem.ClickedCh:
				if applying || rollingBack {
					continue
				}
				// Confirmation gate (issue #108 / E3 #S2-14): a
				// successful apply restarts the tray (~5s of unavailability
				// for the status page and any in-flight menu actions) and
				// is irreversible except by `Roll back`. The default
				// in title/body matches the issue's exact wording.
				if !confirmDestructiveAction(ctx, confirm, confirmDestructive,
					"Confirm install and restart",
					"Apply the downloaded update and restart the tray? The tray will be unavailable for ~5 seconds.") {
					continue
				}
				rel, ok := r.TryLockIdle()
				if !ok {
					refresh()
					continue
				}
				releaseGate = rel
				applying = true
				installItem.Disable()
				installItem.SetTitle("Installing...")
				go func() {
					// A signal-derived ctx cancelling mid-apply could
					// leave a Windows sibling pair at different
					// versions (see internal/selfupdate.Apply's doc
					// comment) -- give the apply its own bounded
					// lifetime independent of ctx's cancellation, while
					// still respecting ctx.Done() as a deadline source.
					applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
					defer cancel()
					version, err := up.ApplyLatest(applyCtx)
					applyDoneCh <- applyResult{version: version, err: err}
				}()
			case res := <-applyDoneCh:
				applying = false
				if res.err != nil {
					releaseGate()
					releaseGate = nil
					installItem.SetTitle("Install and restart (failed -- see status page)")
					installItem.Enable()
					refresh()
					if quitRequested {
						systray.Quit()
						return
					}
					continue
				}
				// Releasing here is belt-and-suspenders: the process is
				// expected to exit and relaunch immediately after this
				// select loop returns, so Runner.gate is abandoned along
				// with everything else either way. Calling it explicitly
				// means that guarantee is never load-bearing -- a future
				// change to the post-success path can't turn this into a
				// silent permanent lock.
				releaseGate()
				releaseGate = nil
				outcome = Outcome{RestartRequested: true, AppliedVersion: res.version}
				systray.Quit()
				return
			case <-rollbackItem.ClickedCh:
				if applying || rollingBack {
					continue
				}
				// Confirmation gate (issue #108 / E3 #S2-14): a
				// successful rollback restarts the tray AND downgrades
				// the running version. Like install, the ~5s restart
				// window and the irreversibility (one more
				// update-and-restart to undo) make it destructive
				// enough to warrant a prompt. The body interpolates
				// the actual current version (the user-visible label
				// the menu also shows) so the operator can verify
				// they're rolling back FROM the right one. us (the
				// up.Status snapshot) is local to refresh() above, so
				// re-read it here for the prompt body -- the version
				// can't change between refresh ticks in a meaningful
				// way for a confirmation, and the alternative is
				// carrying an extra closure variable through every
				// case in this select.
				currentVersion := up.Status().CurrentVersion
				if !confirmDestructiveAction(ctx, confirm, confirmDestructive,
					"Confirm roll back",
					fmt.Sprintf("Roll back to the previous version? Current version: %s.", currentVersion)) {
					continue
				}
				rel, ok := r.TryLockIdle()
				if !ok {
					refresh()
					continue
				}
				releaseGate = rel
				rollingBack = true
				rollbackItem.Disable()
				rollbackItem.SetTitle("Rolling back...")
				go func() {
					// Rollback itself makes no network call (it restores
					// from a local ".previous" backup), but it still gets
					// a bounded, ctx-decoupled lifetime for the same
					// reason ApplyLatest does: a signal-derived shutdown
					// landing mid-swap on a Windows sibling pair would
					// leave the two at different versions.
					rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
					defer cancel()
					version, err := up.Rollback(rbCtx)
					rollbackDoneCh <- applyResult{version: version, err: err}
				}()
			case res := <-rollbackDoneCh:
				rollingBack = false
				if res.err != nil {
					releaseGate()
					releaseGate = nil
					rollbackItem.SetTitle("Roll back (failed -- see status page)")
					rollbackItem.Enable()
					refresh()
					if quitRequested {
						systray.Quit()
						return
					}
					continue
				}
				releaseGate()
				releaseGate = nil
				outcome = Outcome{RestartRequested: true, AppliedVersion: res.version, RolledBack: true}
				systray.Quit()
				return
			case <-ticker.C:
				refresh()
			}
		}
	}

	onExit := func() {
		r.SetOnPauseChange(nil)
		r.SetOnCardIngested(nil)
		r.SetDetectorErrorHandler(nil)
		r.StopDetector()
		close(errCh)
	}

	systray.Run(onReady, onExit)

	select {
	case err, ok := <-errCh:
		if ok {
			return outcome, err
		}
	default:
	}
	return outcome, nil
}

func summarize(st Status) string {
	if st.Paused {
		return "ingest paused by user"
	}
	if st.Busy {
		return fmt.Sprintf("ingesting %s...", st.BusyCard)
	}
	if st.LastIngest == nil {
		return "idle, no ingest run yet"
	}
	li := st.LastIngest
	if li.Err != nil {
		return fmt.Sprintf("last ingest FAILED: %v", li.Err)
	}
	return fmt.Sprintf("last ingest %s ago: %d ok, %d skipped, %d failed",
		time.Since(li.StartedAt).Round(time.Second), li.Submitted, li.Skipped, li.Failed)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// openBrowser shells out to the platform's own "open a URL" command. This
// file only builds for windows/darwin (see the build tag above), so the
// switch's default branch is unreachable in practice; it exists so the
// function still compiles as ordinary, non-platform-specific Go without a
// further per-OS split.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		// rundll32 avoids the cmd.exe quoting pitfalls of "cmd /c start".
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
