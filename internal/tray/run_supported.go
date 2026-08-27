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

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// menuRefreshInterval is how often the informational (disabled) menu items
// re-render from Runner.Status -- covers both a card the watch loop just
// ingested and a self-update note that changed since the tray started.
const menuRefreshInterval = 5 * time.Second

// applyResult is what the install goroutine feeds back to the select loop.
type applyResult struct {
	version string
	err     error
}

// Run starts the tray icon and blocks until ctx is cancelled, the user
// chooses Quit, or an update is installed. detector may be nil to disable
// automatic card-insertion ingest (menu-triggered "Ingest now" against
// watchDirs still works); statusURL is shown in the menu and opened by
// "Open status page". up drives the "Install and restart" affordance --
// Run itself does not know how to check for or apply updates, matching
// Runner's own separation from the ingest core (see tray.go).
func Run(ctx context.Context, r *Runner, detector *ingest.Detector, statusURL string, up SelfUpdater) (Outcome, error) {
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
		systray.AddSeparator()

		watchItem := systray.AddMenuItem("Watch directories: none configured", "Directories polled for inserted cards")
		watchItem.Disable()
		ingestNow := systray.AddMenuItem("Ingest now", "Run one ingest pass over every configured watch directory")
		systray.AddSeparator()
		quitItem := systray.AddMenuItem("Quit", "Stop the branchDAM agent tray")

		if len(r.WatchDirs) == 0 {
			ingestNow.Disable()
		} else {
			watchItem.SetTitle(fmt.Sprintf("Watching %d director%s", len(r.WatchDirs), plural(len(r.WatchDirs))))
		}

		var applying bool

		refresh := func() {
			us := up.Status()
			st := r.Status(us)
			statusItem.SetTitle("Status: " + summarize(st))
			updateItem.SetTitle("Self-update: " + us.Note())

			switch {
			case applying:
				// Left as "Installing..." by the click handler; don't
				// stomp it with a stale UpdateFound-driven title.
			case us.UpdateFound:
				installItem.Show()
				if st.Busy {
					installItem.SetTitle(fmt.Sprintf("Install and restart (waiting for ingest of %s to finish)", st.BusyCard))
					installItem.Disable()
				} else {
					installItem.SetTitle("Install and restart")
					installItem.Enable()
				}
			default:
				installItem.Hide()
			}
		}
		refresh()

		ticker := time.NewTicker(menuRefreshInterval)
		defer ticker.Stop()

		var detectorErrCh chan error
		if detector != nil {
			detectorErrCh = make(chan error, 1)
			go func() {
				detectorErrCh <- detector.Watch(ctx, func(diff ingest.Diff) {
					for _, path := range diff.Inserted {
						r.TriggerIngest(ctx, path)
						refresh()
					}
				})
			}()
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
				for _, dir := range r.WatchDirs {
					r.TriggerIngest(ctx, dir)
				}
				ingestDoneCh <- struct{}{}
			}
		}()

		applyDoneCh := make(chan applyResult, 1)
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
				if applying {
					quitRequested = true
					continue
				}
				systray.Quit()
				return
			case <-quitItem.ClickedCh:
				if applying {
					quitRequested = true
					continue
				}
				systray.Quit()
				return
			case <-openStatus.ClickedCh:
				_ = openBrowser(statusURL)
			case <-ingestNow.ClickedCh:
				select {
				case ingestRequestCh <- struct{}{}:
				default:
					// an ingest request is already queued/running; drop
					// this click rather than pile up requests.
				}
			case <-ingestDoneCh:
				refresh()
			case <-installItem.ClickedCh:
				if applying {
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
			case <-ticker.C:
				refresh()
			case err := <-detectorErrCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					errCh <- err
				}
			}
		}
	}

	onExit := func() {
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
