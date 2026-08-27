# Platform support

The headless subcommands (`preflight`, `ingest`, `queue-drain`, `luminar-sync`, `prune`) run on
Windows, macOS, and Linux. The tray shell (`tray`) is Windows/macOS only.

## Support matrix

| | Windows | macOS (Apple Silicon) | Linux |
|---|---|---|---|
| Headless subcommands | yes | yes | yes |
| Tray shell | yes | yes | no -- `tray: unsupported on this platform` |
| Release binary | `branchdam-agent.exe` + `branchdam-agent-tray.exe` | `branchdam-agent.app` (arm64 only, no amd64) | `branchdam-agent` |

`internal/tray/run_unsupported.go` returns `ErrUnsupported` at runtime on every `GOOS` other
than windows/darwin -- `tray` still builds and runs on Linux, it just errors immediately. A
Linux workstation has the fully-tested headless `ingest` path instead.

## Why Windows ships two binaries

`-H windowsgui` is link-scoped to the *whole* binary, and the same source also serves
`preflight`/`ingest`/`luminar-sync`, whose stdout an operator needs in a console. So
`make build-windows` (and `.github/workflows/release-binaries.yml`) produce two outputs from
one build: `branchdam-agent.exe` (console-linked, for the CLI subcommands) and
`branchdam-agent-tray.exe` (`-H windowsgui`-linked, for the tray/login-item launch path).
`file dist/branchdam-agent-tray.exe` reports `PE32+ executable (GUI)` versus `(console)` for
the other binary.

## `fyne.io/systray`'s cross-compile matrix

`fyne.io/systray` v1.12.2's Windows backend is pure Go (`CGO_ENABLED=0`); its darwin backend
needs cgo (Objective-C) and fails to even compile with `CGO_ENABLED=0`
(`undefined: setInternalLoop` etc, not a linker error) -- and there is no darwin cgo
cross-toolchain on `ubuntu-latest`. That's why `internal/tray` isolates the systray import
behind a `windows || darwin` build tag (`run_supported.go`, with `run_unsupported.go` as the
stub for every other `GOOS`, including Linux, where CI actually builds/tests/lints this repo),
and why the darwin binary is built natively on `macos-26` (both in CI's `build-darwin-full` job
and in `release-binaries.yml`) rather than cross-compiled.

## Settings menu

Issue #31: every tray config change used to require hand-editing `config.yaml` and a full restart.
The tray menu now has a "Settings" submenu, split by what systray itself can render natively
versus what needs an external dialog:

- **Native checkboxes/submenus** (no dialog, no restart): start at login, check for updates, the
  update check interval (1 hour / 24 hours / never), require unbuffered verify.
- **Free-text fields, via a `github.com/ncruces/zenity` dialog** (re-exec'd through the same hidden
  `dialog` subcommand issue #30's startup-error notification uses -- see that section below):
  server URL, API key (a password-style prompt; the current key is never pre-filled or otherwise
  placed in a subprocess's argv), the archive root and local edit root (folder pickers), and the
  naming template.
- **Hand-edit only, on purpose** -- `pathMappings` and `ingest.cardRoots` are both multi-value
  list fields; adding a dialog for editing a list is real additional UI work this PR didn't take
  on. "Open config.yaml" and "Reveal config folder" menu items exist specifically so this path
  never requires knowing where the file lives.

Every change applies through one mechanism, `Runner.Reconfigure` -- see CLAUDE.md's own
"guarded-rebuild" invariant for the full explanation. Two fields can't be hot-reloaded and instead
mark the menu's "Restart now" item visible: `tray.statusAddr` (the status page's bind is this
tray's single-instance guard, already committed to by the time a reload could run) and
`ingest.cardRoots` (the card-detection watch goroutine isn't restartable from inside the running
menu). **Unverified on real hardware**, same caveat as issue #30's dialog work below -- the
settings dialogs share the same re-exec'd `dialog` subcommand.

The embedded status page itself is unchanged by this PR (still the `<meta http-equiv="refresh">`
HTML page from issue #3) -- a `/status.json` route and a smoother live-refresh loop are deferred to
issue #32's tray-timer work, which is already touching this file for the real queue-depth readout.

## Startup diagnostics and first-run setup

Issue #30: a tray launched with no console (`branchdam-agent-tray.exe`, or the macOS `.app` via
LaunchAgent/Dock double-click) had zero user-visible feedback if it failed to start. Two
independent fixes, deliberately not one:

- **A durable log file always lands**, regardless of whether anything can be displayed:
  `internal/agentlog.Setup()` installs an `slog.Logger` writing to both stderr and
  `%LOCALAPPDATA%\branchDAM\logs\agent.log` (Windows), `~/Library/Logs/branchDAM/agent.log`
  (macOS), or `$XDG_STATE_HOME/branchdam-agent/agent.log` (Linux, headless subcommands only --
  the tray itself never runs there). Rotates an existing file past 5MB to a `.1` sibling on next
  start; never blocks startup if it can't be created (falls back to stderr-only).
- **A best-effort error dialog on top**, naming that log path. Rendered by re-exec'ing this same
  binary as the hidden `branchdam-agent dialog` subcommand (`github.com/ncruces/zenity` --
  Win32-native on Windows, `osascript` on macOS, no cgo on any platform) rather than calling
  zenity in-process from the tray -- isolates two platform-specific unknowns neither of which
  could be verified from Linux development: whether a Win32 dialog renders correctly from a
  `-H windowsgui`-linked process before systray's own message pump has started, and whatever
  process-state assumptions a macOS `.app` launched by launchd carries. **Unverified on real
  hardware** -- see Known gaps below.

The same first-run path also covers a *missing* config, not just a broken one: `tray` no longer
exits on a missing `config.yaml`. It writes a starter config (also available headlessly via
`branchdam-agent init`) and, if a dialog backend is available, walks a short setup wizard (server
URL, API key, the two ingest roots) before proceeding. The starter config is left on disk even if
the wizard is canceled or a dialog fails partway, so there is always something to hand-edit
afterward.

## Login-item registration

`internal/autostart/` implements login-item registration for both tray platforms --
`autostart_darwin.go` writes/loads a LaunchAgent plist, `autostart_windows.go` writes an
`HKCU\...\Run` value via `golang.org/x/sys/windows/registry`. Off by default
(`tray.startOnLogin` in config). The plist XML rendering (`autostart.go`, untagged) has unit
tests that run on Linux; the platform-tagged write paths are proven by the
`build-windows`/`build-darwin` cross-compiles and native builds, not by Linux CI directly.

## macOS `.app` bundle

`release-binaries.yml`'s `build-darwin` job assembles `branchdam-agent.app` around the built
binary via `tools/mkbundle` (a thin CLI wrapper over `internal/appbundle`, the package that
actually renders `Info.plist` and lays out `Contents/MacOS/branchdam-agent`) and tars only the
bundle -- never the bare binary alongside it, since two archive entries with the same base name
would leave go-selfupdate's archive-entry selection (see below) to pick whichever it reads
first. `LSUIElement=1` is what's meant to keep the tray out of the Dock and Cmd-Tab switcher;
**this is unverified on real hardware** -- no macOS host has been used interactively to confirm
it. Verify before relying on the macOS tray for day-to-day use. No `.icns` is shipped: with
`LSUIElement=1` there is no Dock tile to show one, and the tray icon itself is already rendered
in Go at startup (`internal/tray/icon.go`) rather than committed as a binary asset.

The bundle is **never code-signed**, deliberately -- see Self-update below for why signing it
would break in-place self-update, and Gatekeeper/quarantine below for what that means for a
first manual install.

### Gatekeeper, quarantine, and translocation

A browser download typically sets the `com.apple.quarantine` extended attribute on the
downloaded archive's contents; launching an unsigned, un-notarized app from a quarantined,
non-standard location (`~/Downloads` in particular) can trigger macOS's App Translocation,
which runs the bundle from a randomized **read-only** mount
(`/private/var/folders/.../AppTranslocation/...`). `internal/selfupdate.DetectLayout` refuses
(`ErrTranslocated`) rather than silently fail or register a login item at a path that
disappears on reboot. **Install by moving `branchdam-agent.app` to `/Applications` or
`~/Applications` in Finder** -- this both clears the translocation and is required anyway for
self-update to have write access to the install directory (see Self-update below). Downloading
via `curl`/`tar` in a terminal instead of a browser avoids the quarantine attribute entirely, if
preferred.

## Self-update

Checking for an update is **on by default** (`selfUpdate.enabled: true` is the Go-level and
`config.example.yaml` default) -- no build tag required, compiled into every build. Checking is
a read-only GitHub API call; it never downloads or writes anything. Set `selfUpdate.enabled:
false` for zero outbound GitHub traffic. `internal/selfupdate.Updater` always configures a
`ChecksumValidator` against the release's `SHA256SUMS.txt`; nothing in this repo's release
pipeline is code-signed or notarized, so that checksum check is the *only* integrity control
before a downloaded binary is written to disk.

Notify-and-confirm, never unattended: the tray checks on startup and periodically thereafter
(`selfUpdate.checkIntervalHours`, default 24h) and shows "Install and restart" once an update is
found, but nothing downloads or applies until that menu item is clicked (or, on a headless host,
`branchdam-agent update` is run and confirmed) -- `selfUpdate.enabled` gates whether the binary
may contact GitHub at all, not whether an update, once found, gets applied automatically; that
second gate is always a separate, explicit action regardless of this flag. The install refuses
while an ingest is in flight, for the whole duration of the download and apply, not just at the
moment the click is handled -- `internal/tray.Runner.TryLockIdle` holds the same gate
`TriggerIngest` does.

**Self-update requires a per-user install location.** Applying writes a replacement binary next
to the running one, which needs write permission on the containing directory --
`/Applications` or `C:\Program Files\` are not writable without elevation.  Install to
`~/Applications` (macOS) or `%LOCALAPPDATA%\Programs\branchDAM\` (Windows); a system-wide
install can still be updated, just not by itself -- reinstall the release archive instead.

On Windows, one apply replaces both `branchdam-agent.exe` and `branchdam-agent-tray.exe` --
`internal/selfupdate.DetectLayout` finds the sibling and `Updater.Apply` applies to it first,
the running exe last, so a failure partway through never leaves the two at different versions.
On macOS, go-selfupdate's archive-entry selection matches an archive member by base name
regardless of its path inside the archive, so it extracts and replaces
`branchdam-agent.app/Contents/MacOS/branchdam-agent` without ever touching the bundle itself;
`Apply` rewrites `Info.plist` locally right after (via the same `internal/appbundle` renderer
`tools/mkbundle` uses at build time) so `CFBundleVersion` doesn't go stale. This is why the
bundle is never code-signed: signing binds the signature to the bundle's contents, and
self-update mutating the inner binary afterward would make macOS refuse to launch the result.

`go-selfupdate` v1.6.0 imports `golang.org/x/crypto/openpgp` unconditionally from its top-level
package, which `govulncheck` flags as `GO-2026-5932` (unfixed, no upgrade path -- v1.6.0 is the
newest tag). The `test-go / lint-and-test` check suppresses that specific finding via
`s3ntin3l8/.github/ci-go.yml`'s `govulncheck-ignore` input. See tracked issue #14 for when the
ignore entry can be dropped.

## Known gaps

- **`LSUIElement=1` Dock-suppression is unverified on real hardware**, as is whether AppKit's
  bundle discovery still honors it when launchd execs the bundle's inner binary directly rather
  than opening the `.app` -- see the macOS `.app` bundle section above.
- **The exact Gatekeeper prompt wording, and whether App Translocation actually triggers the way
  described above, are unverified** without a macOS host to observe them directly.
- **The Windows two-binary self-update apply and the macOS relaunch-via-`open -n -a` path are
  both unverified end-to-end** -- `internal/selfupdate`'s archive-entry-selection and checksum
  logic are pinned by tests that run on Linux CI (see that package's tests), but a full
  download-verify-swap-relaunch cycle has not been exercised on real Windows or macOS hardware.
- **The startup-error dialog (issue #30) is unverified on real hardware.** Does it actually appear
  when re-exec'd from `branchdam-agent-tray.exe` (a `-H windowsgui`-linked process with no
  console, before systray's own message pump has started), and from the `.app` bundle launched by
  launchd? `dialog.go`'s flag parsing, exit-code mapping, and the first-run wizard's prompt/cancel
  logic are all pinned by tests that run on Linux CI, substituting a fake for the actual
  `zenity` call -- but nothing here has exercised a real Win32 dialog or a real launchd-spawned
  `osascript` call. The durable log file (`internal/agentlog`) is independent of this and does not
  share the gap.
- **The settings menu's dialogs (issue #31) share the startup-error dialog's exact unverified
  status**, for the same reason -- they go through the same re-exec'd `dialog` subcommand. What's
  additionally unverified: whether a Win32 dialog renders correctly while systray's own message
  pump is *already running* (the startup-error dialog fires before `systray.Run` starts;
  settings dialogs fire from a running tray's click handler -- a materially different scenario the
  original design doc flagged and this PR did not spike separately). `configSettings`'s own logic
  (persistence, reload, restart-required diffing) is pinned by tests substituting a fake
  `dialogRunner`; the dialogs' actual rendering is not.
- **Tray queue status has no live rate/ETA or in-progress-transfer readout (issue #32).** The
  status page and menu show a real `internal/queue.Store.Counts` snapshot (pending, permanently
  failed, done) plus the last completed drain/prune pass's summary -- but not a live "copying
  X.jpg, 40% done" or a bytes/sec rate while a drain's archive copy is in flight, even though
  `internal/ingest`'s progress-callback plumbing (`ProgressEvent`) already exists and could feed
  one. Deferred as its own follow-on: it needs a progress callback threaded through a
  concurrently-running `Drain` pass with its own synchronization against `Runner`'s polled
  `Status()` calls, which is meaningfully more design surface than the Counts-based readout this
  PR shipped. `offline.drainIntervalSecs`/`prune.intervalMinutes` are also config-file-only for
  now -- not yet exposed in the tray's settings menu.
- **Tray-driven drain/prune passes are unverified on real hardware.** `Runner.TriggerDrain`/
  `TriggerPrune`'s locking (a dedicated mutex for drain, sharing the ingest/self-update gate for
  prune) and the two background timers (`cmd/branchdam-agent/queueagent.go`'s `startPeriodic`) are
  pinned by unit tests with fake `Drainer`/`Pruner`s, but nothing here has exercised a real
  drain/prune pass running concurrently with a real card ingest on Windows or macOS.
- **The Luminar `catalog.db` query is unvalidated against a real catalog.** Luminar's schema is
  undocumented; see [`luminar-catalog.md`](luminar-catalog.md) for the research behind the
  built-in query, its confidence level, and `--dump-schema` for correcting it against your own
  catalog.
- **`prune` is not real Tier-1 NLE scratch pruning.** It only ever considers files ingested via
  `ingest -offline` (rows in `queue.db`) -- a plain online `ingest` has no durable local-path
  ledger to prune against. Real Tier-1 `LOCAL_SCRATCH` pruning stays architecturally blocked on
  branchDAM's side (branchDAM issues #230, #266): nothing can mint a `storage_location_id` for
  an unmounted tier without breaking that invariant.
