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
	"image/color"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"fyne.io/systray"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
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

// applyResult is what the install goroutine feeds back to the select loop.
type applyResult struct {
	version string
	err     error
}

// menuAction pairs one blocking Settings call with the menu item that owns
// its error -- settingsMenu is the sole producer today (see onReady
// below), routed through one worker goroutine/channel so a plain func()
// error is no longer enough to route a result back to the RIGHT lastErr
// field. Before the per-integration/per-hook tray submenus were removed
// in favor of the Wails window's equivalent actions (issue #211's
// follow-through), integrationsMenu and each of its per-registry-entry
// integrationSubmenus fed this same channel; the pair-of-funcs shape is
// kept even with one producer, since a second config-changing tray menu
// would need it again. The worker goroutine must never call report
// itself: report mutates a *systray.MenuItem-owning struct's own field,
// which sync() (called from Run's own select loop) also reads -- calling
// report from any other goroutine would race. It is therefore called ONLY
// from the select loop's menuDoneCh case, never from the worker.
type menuAction struct {
	run    func() error
	report func(error)
}

// menuActionResult is what the shared worker goroutine feeds back --
// report is carried through unchanged so the select loop can invoke it
// without needing to know which menu originated the action.
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
// r drives the tray's state snapshot and ingest triggers. up drives the
// "Install and restart" affordance; settings drives the "Advanced" submenu
// (issue #31) -- Run itself does not know how to check for updates or
// persist config, matching Runner's own separation from the ingest core
// (see tray.go).
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
	up SelfUpdater,
	settings Settings,
	confirm func(ctx context.Context, title, body string) bool,
	confirmDestructive bool,
	pickDir func(ctx context.Context, title string) (string, error),
	notify func(ctx context.Context, title, message string),
) (Outcome, error) {
	errCh := make(chan error, 1)
	var outcome Outcome
	r.SetConfirmDestructive(confirmDestructive)

	onReady := func() {
		if runtime.GOOS == "darwin" {
			// Template icon: macOS reads the alpha channel and
			// auto-tints the foreground to match the menu bar.
			// SetTemplateIcon (not SetIcon) is required for this.
			systray.SetTemplateIcon(buildTemplateIcon(), buildTrayIcon())
			systray.SetTitle("")
		} else {
			systray.SetIcon(buildTrayIcon())
			systray.SetTitle("branchDAM")
		}
		systray.SetTooltip("branchDAM agent")

		statusItem := systray.AddMenuItem("Status: starting...", "Current tray status")
		statusItem.Disable()
		openUI := systray.AddMenuItem("Open branchDAM", "Open the native branchDAM window -- the single settings/status surface")
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
		// prompt/Reload/Open/Reveal -- runs through this ONE shared
		// worker, for the same reason ingestNow does: they all do
		// blocking I/O (disk, a dialog subprocess, rebuilding the ingest
		// Engine), and running any of that inline in the select loop
		// below would freeze the whole menu, including Quit, for as long
		// as it takes. settingsMenu's own dispatch goroutine feeds this
		// channel directly (non-blocking, drops a click if one is already
		// in flight) rather than routing through the select loop below.
		// See menuAction's own doc comment for why the worker never calls
		// report itself.
		//
		// Prior to the per-integration/per-hook tray submenus being
		// removed in favor of the Wails window's equivalent actions
		// (issue #211's follow-through), Integrations' own
		// checkbox/catalog-path/interval clicks fed this same channel
		// too -- settingsMenu is the sole producer now.
		menuActionCh := make(chan menuAction, 1)
		menuDoneCh := make(chan menuActionResult, 1)
		go func() {
			for a := range menuActionCh {
				menuDoneCh <- menuActionResult{report: a.report, err: a.run()}
			}
		}()

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

			// pauseItem's title/check/tooltip track st.Paused unconditionally --
			// independent of whether config is complete, since an operator can
			// toggle pause (a session-only, in-memory gate) at any time,
			// including mid-setup. Only the tray icon and its own tooltip are
			// chosen three-way below: ConfigIncomplete takes priority over
			// Paused for the icon because "not configured" is the more
			// actionable state to surface at a glance.
			if st.Paused {
				pauseItem.Check()
				pauseItem.SetTitle("▶ Resume ingest")
				pauseItem.SetTooltip("Resume automatic card detection and queue draining")
			} else {
				pauseItem.Uncheck()
				pauseItem.SetTitle("⏸ Pause ingest")
				pauseItem.SetTooltip("Temporarily suspend automatic card detection and queue draining")
			}

			switch {
			case st.ConfigIncomplete:
				if runtime.GOOS == "darwin" {
					systray.SetTemplateIcon(buildTemplateIcon(), buildTrayIcon())
				} else {
					systray.SetIcon(buildUnconfiguredTrayIcon())
				}
				systray.SetTooltip("branchDAM agent — not configured")
			case st.Paused:
				systray.SetIcon(buildPausedTrayIcon())
				systray.SetTooltip("branchDAM agent (ingest paused)")
			default:
				if runtime.GOOS == "darwin" {
					systray.SetTemplateIcon(buildTemplateIcon(), buildTrayIcon())
				} else {
					systray.SetIcon(buildTrayIcon())
				}
				systray.SetTooltip(FormatTooltip(st))
			}

			// Watch dirs can change out from under this menu now that
			// Reconfigure exists (issue #31's settings menu) -- re-render
			// on every tick rather than only at startup.
			if st.ConfigIncomplete {
				watchItem.SetTitle("Watch directories: not configured")
				ingestNow.Disable()
			} else if len(st.WatchDirs) == 0 {
				watchItem.SetTitle("Watch directories: none configured")
				ingestNow.Disable()
			} else {
				watchItem.SetTitle(fmt.Sprintf("Watching %d director%s", len(st.WatchDirs), plural(len(st.WatchDirs))))
				ingestNow.Enable()
			}

			qs := st.QueueStatus
			switch {
			case st.ConfigIncomplete:
				queueItem.SetTitle("Queue: not configured")
				drainNow.Disable()
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
			if !st.ConfigIncomplete && qs.Configured && qs.PruneEnabled {
				pruneNow.Enable()
			} else {
				pruneNow.Disable()
			}

			if drainSkipped {
				drainNow.SetTitle("Drain queue now (skipped -- already running, metered, or paused)")
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
			if sv.RestartRequired {
				restartNowItem.Show()
			} else {
				restartNowItem.Hide()
			}

			if sv.PauseUploadOnMetered {
				if metered, mErr := netgate.IsMetered(); metered || mErr != nil {
					if mErr != nil {
						slog.Debug("metered probe failed for tooltip", "err", mErr)
					}
					systray.SetTooltip("Upload paused (metered connection)")
				} else {
					systray.SetTooltip("branchDAM agent")
				}
			} else {
				systray.SetTooltip("branchDAM agent")
			}

			switch {
			case applying:
				// Left as "Installing..." by the click handler; don't
				// stomp it with a stale UpdateFound-driven title.
			case us.Phase == UpdatePhaseChecking || us.Phase == UpdatePhaseDownloading ||
				us.Phase == UpdatePhaseVerifying || us.Phase == UpdatePhaseRestarting:
				installItem.Show()
				installItem.SetTitle(fmt.Sprintf("Install and restart (%s)", us.Phase))
				installItem.Disable()
			case us.Phase == UpdatePhaseFailed && !us.StartedAt.IsZero():
				installItem.Show()
				installItem.SetTitle("Install and restart (failed -- see branchDAM window)")
				installItem.Enable()
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
			case <-openUI.ClickedCh:
				// No focus-existing-window logic needed here: Wails'
				// SingleInstanceLock (cmd/branchdam-agent-ui/main.go) already
				// brings a running window forward on a second launch, so
				// this always just starts the binary and lets Wails decide.
				if err := launchUIBinary(); err != nil {
					openUI.SetTitle(fmt.Sprintf("Open branchDAM (failed: %v)", err))
				} else {
					openUI.SetTitle("Open branchDAM")
				}
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
				if !confirmDestructiveAction(ctx, confirm, r.ConfirmDestructive(),
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
				if !confirmDestructiveAction(ctx, confirm, r.ConfirmDestructive(),
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
				if !confirmDestructiveAction(ctx, confirm, r.ConfirmDestructive(),
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
					installItem.SetTitle("Install and restart (failed -- see branchDAM window)")
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
				if !confirmDestructiveAction(ctx, confirm, r.ConfirmDestructive(),
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
					rollbackItem.SetTitle("Roll back (failed -- see branchDAM window)")
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
				// A window-triggered apply runs outside this select loop. Once
				// it has completed, the updater marks the phase restarting;
				// make the same orderly shutdown/relaunch decision as the tray
				// menu apply handler. Keep this in the select-loop goroutine:
				// refresh is also invoked by detector callbacks.
				us := up.Status()
				if us.Phase == UpdatePhaseRestarting && !applying && !rollingBack {
					outcome = Outcome{RestartRequested: true, AppliedVersion: us.Applied}
					systray.Quit()
					return
				}
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

// buildTemplateIcon renders the monogram in white — the alpha channel
// carries the shape, and macOS uses it for auto-tinting via
// SetTemplateIcon. The RGB values are irrelevant; macOS ignores them.
func buildTemplateIcon() []byte {
	return buildIconColor(false, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
}

// buildUnconfiguredTrayIcon renders branchDAM's b-node monogram in gray
// on Windows, indicating the tray is running with an incomplete config.
func buildUnconfiguredTrayIcon() []byte {
	return buildIconColor(false, color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff}) // gray
}

// summarize returns a one-line status string for the tray menu item.
// When config is incomplete, it intentionally omits the missing field
// names — those are shown in detail in the Wails Settings window's setup
// banner. The tray menu needs only a quick pointer to Settings, not a
// wall of technical field names.
func summarize(st Status) string {
	if st.ConfigIncomplete {
		return "not configured"
	}
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
	if li.Offline {
		if li.Failed == 0 && li.Skipped == 0 {
			if li.Submitted == 1 {
				return fmt.Sprintf("last ingest %s ago: 1 file queued offline, pending upload",
					time.Since(li.StartedAt).Round(time.Second))
			}
			return fmt.Sprintf("last ingest %s ago: %d files queued offline, pending upload",
				time.Since(li.StartedAt).Round(time.Second), li.Submitted)
		}
		return fmt.Sprintf("last ingest %s ago (offline): %d queued offline, %d skipped, %d failed",
			time.Since(li.StartedAt).Round(time.Second), li.Submitted, li.Skipped, li.Failed)
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

// uiBinaryName is cmd/branchdam-agent-ui's built binary's platform-specific
// basename, both halves imported from internal/appbundle rather than
// re-spelled as literals here -- appbundle is a lightweight, pure-Go leaf
// package (fmt/io/os/path/filepath/regexp only) both internal/selfupdate
// and this package can safely depend on, unlike internal/selfupdate
// itself, which SelfUpdater's own doc comment above already established is
// deliberately NOT imported into this package (it pulls in
// golang.org/x/crypto/openpgp transitively). A Hermes review finding on PR
// #218: this function used to duplicate the Windows name as an independent
// literal, which could silently drift from internal/selfupdate's own
// winUIExe if the binary were ever renamed -- appbundle.WinUIBinaryName is
// now that package's own single source of truth for the same reason
// UIBinaryName already was for the macOS name.
func uiBinaryName() string {
	if runtime.GOOS == "windows" {
		return appbundle.WinUIBinaryName
	}
	return appbundle.UIBinaryName
}

// launchUIBinary starts cmd/branchdam-agent-ui as a detached, non-blocking
// process (issue #211). It never checks whether one is already running --
// Wails' own SingleInstanceLock (cmd/branchdam-agent-ui/main.go) already
// handles that: a second launch focuses the existing window instead of
// opening a duplicate.
func launchUIBinary() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	uiPath, err := resolveUIBinaryPath(exe)
	if err != nil {
		return err
	}
	return exec.Command(uiPath).Start()
}

// resolveUIBinaryPath resolves execPath (the running tray's own executable
// path, i.e. os.Executable()'s result) through symlinks and returns its
// sibling UI binary's path -- split out from launchUIBinary so this
// resolution logic is unit-testable against a synthetic execPath, without
// needing to fake os.Executable() itself, mirroring
// internal/selfupdate.DetectLayout's own execPath-as-parameter shape for
// exactly the same reason.
//
// The UI binary is expected as a sibling of execPath in the same directory
// -- true for both the Windows installer's $INSTDIR (all three .exe's
// alongside each other) and the macOS .app bundle's Contents/MacOS/
// (Track 3f's packaging PR put it there on both platforms; see
// internal/appbundle.Write's uiBinPath parameter and
// installer/windows/branchdam-agent.nsi's own File command). An install
// predating Track 3f, or a dev build with no UI binary at all, reports a
// clear error instead of silently doing nothing.
func resolveUIBinaryPath(execPath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return "", fmt.Errorf("resolve own executable path: %w", err)
	}
	uiPath := filepath.Join(filepath.Dir(resolved), uiBinaryName())
	if _, err := os.Stat(uiPath); err != nil {
		return "", fmt.Errorf("not found at %s (install predates packaging, or a dev build?)", uiPath)
	}
	return uiPath, nil
}
