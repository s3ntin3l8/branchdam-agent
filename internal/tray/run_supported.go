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

// Run starts the tray icon and blocks until ctx is cancelled or the user
// chooses Quit from the menu. detector may be nil to disable automatic
// card-insertion ingest (menu-triggered "Ingest now" against watchDirs
// still works); statusURL is shown in the menu and opened by "Open status
// page". selfUpdateNote is a short, pre-computed status line (e.g. "up to
// date" / "disabled" / "update available: vX.Y.Z") -- Run does not itself
// know how to check for updates, matching Runner.Status's own separation
// (see tray.go).
func Run(ctx context.Context, r *Runner, detector *ingest.Detector, statusURL string, selfUpdateNote func() string) error {
	errCh := make(chan error, 1)

	onReady := func() {
		systray.SetIcon(buildTrayIcon())
		systray.SetTitle("branchDAM")
		systray.SetTooltip("branchDAM agent")

		statusItem := systray.AddMenuItem("Status: starting...", "Current tray status")
		statusItem.Disable()
		openStatus := systray.AddMenuItem("Open status page", statusURL)
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

		refresh := func() {
			st := r.Status(selfUpdateNote())
			statusItem.SetTitle("Status: " + summarize(st))
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

		for {
			select {
			case <-ctx.Done():
				systray.Quit()
				return
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			case <-openStatus.ClickedCh:
				_ = openBrowser(statusURL)
			case <-ingestNow.ClickedCh:
				for _, dir := range r.WatchDirs {
					r.TriggerIngest(ctx, dir)
				}
				refresh()
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
			return err
		}
	default:
	}
	return nil
}

func summarize(st Status) string {
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
