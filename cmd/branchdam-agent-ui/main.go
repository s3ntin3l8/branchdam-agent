//go:build windows || darwin

// Command branchdam-agent-ui is the native status window for branchDAM's
// tray agent (Track 3c of the distribution/UX plan -- see
// docs/platform-support.md). A separate binary from the tray, not a
// feature bundled into it: both fyne.io/systray and Wails need the
// platform's own GUI main-loop on macOS, and only one process can own
// that loop, so the window talks to the tray over the loopback API
// (internal/tray/statusapi.go) rather than sharing a process with it.
//
// This file deliberately never calls the `wails` CLI's own build/dev
// commands -- Wails' binding glue (window.go.main.App.* in
// frontend/dist/app.js) is injected by this package's own wails.Run at
// window-load time via runtime reflection over Bind, not generated ahead
// of time, so a plain `go build` produces a fully working binary. The
// generated TypeScript types the CLI would otherwise produce under
// frontend/wailsjs/ are an IDE convenience this hand-written vanilla-JS
// frontend has no use for.
package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

// singleInstanceID scopes the single-instance lock to this binary alone --
// Wails' lock is keyed by this string process-wide on the host, so it must
// never collide with an unrelated app sharing the same machine.
const singleInstanceID = "com.branchdam.agent.ui"

// version is stamped at build time via -ldflags "-X main.version=...",
// mirroring cmd/branchdam-agent's own var of the same name -- see
// .github/workflows/release-binaries.yml's build-windows/build-darwin
// jobs. "dev" for a local unstamped build. Shown in the title bar (visible
// even before the page loads) and via the bound App.Version method (so
// the frontend can show it next to the agent's own reported version --
// see app.js's render() -- since the two binaries can be at different
// versions if one side of a self-update fails partway; see
// internal/selfupdate.InstallLayout's own Siblings ordering for why that
// window should be brief, not why it can't happen at all).
var version = "dev"

func main() {
	app := NewApp()
	err := wails.Run(&options.App{
		Title:       "branchDAM (" + version + ")",
		Width:       960,
		MinWidth:    640,
		Height:      720,
		MinHeight:   480,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   app.Startup,
		Bind:        []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: singleInstanceID,
			// A second launch (double-clicking the app again, or a
			// future "Open branchDAM" tray menu item once 3f's
			// packaging gives it somewhere to find this binary) should
			// bring the existing window forward rather than opening a
			// second one -- there is only ever one agent to look at.
			OnSecondInstanceLaunch: func(_ options.SecondInstanceData) {
				// app.ctx is nil until OnStartup fires; a second launch
				// landing in that window (lock acquired, window not yet
				// up) would otherwise panic here.
				if app.ctx != nil {
					runtime.WindowShow(app.ctx)
				}
			},
		},
	})
	if err != nil {
		println("branchdam-agent-ui: " + err.Error())
	}
}
