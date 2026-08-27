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

Gated by `selfUpdate.enabled: true` in config -- no build tag required, compiled into every
build. `internal/selfupdate.Updater` always configures a `ChecksumValidator` against the
release's `SHA256SUMS.txt`; nothing in this repo's release pipeline is code-signed or
notarized, so that checksum check is the *only* integrity control before a downloaded binary is
written to disk.

Notify-and-confirm, never unattended: the tray checks on startup and periodically thereafter
(`selfUpdate.checkIntervalHours`, default 24h) and shows "Install and restart" once an update is
found, but nothing downloads or applies until that menu item is clicked (or, on a headless host,
`branchdam-agent update` is run and confirmed). The install refuses while an ingest is in
flight, for the whole duration of the download and apply, not just at the moment the click is
handled -- `internal/tray.Runner.TryLockIdle` holds the same gate `TriggerIngest` does.

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
- **Tray status page's queue field is a stub.** The embedded status page
  (`http://127.0.0.1:38080/` by default, loopback-only) shows queue status as
  `tray.QueueStatusStub`, a literal placeholder string, not a real count -- the offline queue
  isn't wired into the status page's display yet.
- **The Luminar `catalog.db` query is unvalidated against a real catalog.** Luminar's schema is
  undocumented; see [`luminar-catalog.md`](luminar-catalog.md) for the research behind the
  built-in query, its confidence level, and `--dump-schema` for correcting it against your own
  catalog.
- **`prune` is not real Tier-1 NLE scratch pruning.** It only ever considers files ingested via
  `ingest -offline` (rows in `queue.db`) -- a plain online `ingest` has no durable local-path
  ledger to prune against. Real Tier-1 `LOCAL_SCRATCH` pruning stays architecturally blocked on
  branchDAM's side (branchDAM issues #230, #266): nothing can mint a `storage_location_id` for
  an unmounted tier without breaking that invariant.
