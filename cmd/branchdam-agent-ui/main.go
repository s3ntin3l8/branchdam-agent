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
// of time. The generated TypeScript types the CLI would otherwise produce
// under frontend/wailsjs/ are an IDE convenience this hand-written
// vanilla-JS frontend has no use for.
//
// EVERY build of this package MUST pass -tags production. Without it,
// wails v2.16.0's own `//go:build !dev && !production && !bindings` guard
// (internal/app/app_default_windows.go / app_default_unix.go) substitutes
// a stub CreateApp: on Windows that pops a "Wails applications will not
// build without the correct build tags" MessageBox; on macOS it returns an
// error that main below now logs via agentlog (see main's own comment for
// why that replaced a bare println), so the window still silently never
// appears on screen, but the failure is at least diagnosable after the
// fact. The tag is
// self-contained -- wails embeds its production runtime JS
// (internal/frontend/runtime/runtime_prod_desktop.go) from the module
// itself, so no `wails` CLI and no npm step are required, and it is
// CGO-neutral (the Windows leg still cross-compiles with
// CGO_ENABLED=0). The five sites that must carry it: Makefile's
// build-windows and build-darwin-app targets, release-binaries.yml's
// build-windows and build-darwin jobs, and ci-cd.yml's build-darwin-full
// job (which exists solely to typecheck this build on a real macOS host
// before release, since darwin has no visible failure mode to catch a
// missing tag in the field). Turning the tag on for the first time on a
// real macOS host surfaced a separate, genuine upstream gap in wails
// v2.16.0's own darwin cgo linking -- see cgo_darwin.go's own doc
// comment.
package main

import (
	"embed"
	"log/slog"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
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
	// A Hermes review finding on this PR: the missing-build-tag failure
	// this file's own doc comment above describes (wails' stub CreateApp
	// returning an error on macOS) had no trace anywhere a user could
	// find it -- println writes to a stdout nobody reads for a
	// double-clicked .app or a launchd-started process. agentlog.Setup
	// installs the same durable-log-file-plus-stderr default logger
	// cmd/branchdam-agent's tray.go uses (internal/agentlog's own doc
	// comment: built specifically for "a `-H windowsgui`-linked tray or a
	// macOS `.app` launched by launchd, neither of which has anywhere for
	// stderr to go"), so a mis-tagged build now leaves a diagnosable
	// footprint on the user's machine, not just in CI.
	_, closeLog, logErr := agentlog.Setup()
	defer func() { _ = closeLog() }()
	if logErr != nil {
		// Non-fatal, matching tray.go's own precedent: agentlog.Setup
		// already fell back to an stderr-only default logger.
		slog.Warn("could not set up durable logging", "err", logErr)
	}

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
		Mac: &mac.Options{
			// A branchdam:// link clicked while this window is open is
			// delivered here (GetURL Apple Event), not to the tray.
			// Hand it to the tray, which owns the confirm + pair flow.
			OnUrlOpen: app.HandleOpenURL,
		},
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
		slog.Error("branchdam-agent-ui exited with an error", "err", err)
	}
}
