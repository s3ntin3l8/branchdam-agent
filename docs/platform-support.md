# Platform support

The headless subcommands (`preflight`, `ingest`, `queue-drain`, `luminar-sync`, `prune`) run on
Windows, macOS, and Linux. The tray shell (`tray`) is Windows/macOS only.

## Support matrix

| | Windows | macOS (Apple Silicon) | Linux |
|---|---|---|---|
| Headless subcommands | yes | yes | yes |
| Direct HTTP Streaming Ingest (`-upload`) | yes | yes | yes |
| Tray shell | yes | yes | no -- `tray: unsupported on this platform` |
| Release binary | `branchdam-agent.exe` + `branchdam-agent-tray.exe` | `branchdam-agent.app` (arm64 only, no amd64) | `branchdam-agent` |

`internal/tray/run_unsupported.go` returns `ErrUnsupported` at runtime on every `GOOS` other
than windows/darwin -- `tray` still builds and runs on Linux, it just errors immediately. A
Linux workstation has the fully-tested headless `ingest` path instead.

## Direct HTTP streaming & server-governed naming

In addition to traditional dual-writing to local filesystem mounts (SMB/NFS shares), the agent supports direct streaming uploads across all platforms:
- **`POST /api/v1/agent/upload`**: Ingest directly into the server's Master Archive over HTTP/HTTPS.
- **Handshake Synchronization (`POST /api/v1/agent/handshake`)**: The server returns the active `namingTemplate`, which the workstation agent synchronizes at startup to ensure 100% path parity between local NVMe edit scratch (`LocalEditRoot`) and remote server archive storage.
- **Cryptographic Verification**: The server computes and returns the Master Archive's BLAKE3 checksum, which the agent verifies against the local copy prior to granting safe card ejection.

## Soft-delete trash lifecycle

Asset deletions triggered via `EVENT_NODE_DELETED` do not immediately purge files from disk. Instead, branchDAM isolates the asset in the `.trash/` directory under `TIER3_MASTER_ARCHIVE` with a 30-day safety retention window (`trash.retentionDays`), while immediately purging references from active galleries and database indices.

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
  update check interval (1 hour / 24 hours / never), require unbuffered verify, **Require DCIM
  folder** (#81, skips volumes without a `DCIM/` subdirectory), **Auto-eject after ingest** (#87,
  unmounts and ejects the card volume after a verified ingest — gated on the ingest summary
  being `OK()`), **Pause upload on metered connection** (#84, short-circuits `TriggerDrain` on
  a hotspot or metered link without blocking the local edit copy), **Pause ingest** (#83,
  shoot-mode: short-circuits `TriggerIngest` / `TriggerDrain` / `TriggerPrune` and drops detector
  events; session-only, not persisted).
- **Free-text fields, via a `github.com/ncruces/zenity` dialog** (re-exec'd through the same hidden
  `dialog` subcommand issue #30's startup-error notification uses -- see that section below):
  server URL, API key (a password-style prompt; the current key is never pre-filled or otherwise
  placed in a subprocess's argv), the archive root and local edit root (folder pickers), the
  naming template, **Watch folders…** (#78, multi-value directory picker for `ingest.cardRoots`),
  and **Allowed extensions…** (#81, comma-separated `ingest.allowedExtensions` allow-list).
- **List-editing dialog** (separate from the free-text dialog because multi-value lists need a
  different zenity kind) — `ingest.autoImportPaths` (#79, the allow-list that bypasses the
  confirmation dialog for known card volumes) is edited through the same `SetStringSlice`
  path the headless `settings.go` exposes.
- **Hand-edit only, on purpose** -- `pathMappings` remains hand-edit (deliberate: it requires
  operator judgement about container paths). `ingest.cardRoots` was previously in this category
  but graduated to the free-text dialog by #78 alongside a live `Runner.ReconfigureDetector`
  path so changing it does not require a process restart. "Open config.yaml" and "Reveal config
  folder" menu items remain for any field that ever needs a hand-edit escape hatch.

Every change applies through one mechanism, `Runner.Reconfigure` -- it rebuilds the
affected components (`branchdam.Client`, `ingest.Engine`) and applies them atomically under a
mutex lock. A field that requires a restart (`tray.statusAddr` is the only remaining one;
`ingest.cardRoots` is hot-reloaded by `ReconfigureDetector`) is caught by a snapshot diff in
`SettingsView`, which shows "Restart now" instead of applying immediately. **Unverified on real
hardware**, same caveat as issue #30's dialog work below -- the settings dialogs share the same
re-exec'd `dialog` subcommand.

The embedded status page (still the `<meta http-equiv="refresh">` HTML page from issue #3, no JS) now
also renders an "Integrations" section (per-integration enabled/dry-run config state joined by ID
against the last sync summary -- see the `integrationView` template func in
`internal/tray/statusserver.go`) and a "DaVinci Resolve render hook" section (issue #60/#61) -- see
the Status page section below. A `/status.json` route and a smoother live-refresh loop are still
deferred to issue #32's own tray-timer work. Live per-file ingest progress
(per-file path, bytes done / total, phase, and elapsed time) is already
shipped by #85: it renders in the tray tooltip while a card is being
ingested and on the status page under the same busy-card header. See
`internal/ingest/progress.go`'s `ProgressEvent` and `internal/tray/tooltip.go`'s
`FormatIngestProgress`.

## Integrations menu

A separate top-level menu, not nested under Settings: one item per catalog integration
(`internal/tray.Integrations()`'s compile-time registry -- Luminar Neo today), plus a shared "Node
index…" item. Each integration's own submenu -- Enabled, Dry run, Catalog…, "Sync every" (its own
nested submenu: 15 min / 60 min default / manual-only), "Sync now" -- keeps every leaf at depth 3
(`Luminar Neo ▸ Sync every ▸ 15 minutes`), matching the deepest tree this repo has actually shipped
(`Settings ▸ Check every ▸ 1 hour`) rather than nesting a fourth level under a wrapper "Integrations"
item.

**Different mechanism from Settings.** Settings changes apply through `Runner.Reconfigure`;
integration changes apply through the separate `Runner.SetIntegrationSyncers` (rebuilt on every
reload, same staleness class of fix as the offline-queue drainer/pruner rebuild). "Sync now" is a
`Runner` action (like "Drain queue now"), not a Settings mutation -- it goes through
`Runner.TriggerSync` directly from `run_supported.go`'s own select loop, one worker goroutine per
registry entry.

**Shared error-reporting channel, split lastErr.** Settings and Integrations share one worker
goroutine/channel (`menuActionCh`/`menuDoneCh` in `run_supported.go`) since both do blocking I/O and
neither can run inline without freezing the whole menu (including Quit) -- but each menu (and each
integration's own submenu) owns its **own** `lastErr`, routed via a `report` callback carried on the
action itself (`menuAction`/`menuActionResult`), so a Settings action's error can never appear in an
Integrations submenu's title or vice versa. A consequence worth knowing: the channel itself is
shared and size-1, so a Settings click and an Integrations click landing in the same instant means
one is dropped -- the same "drop rather than queue" semantics every other menu action in this file
already has.

**Unverified on real hardware**, same caveat as the Settings menu above and issue #30's dialog work
below: the catalog/node-index file picker (`-kind file` in the `dialog` subcommand) has never
rendered on a real Win32 message pump or via `osascript`, and the four-registry-entry-worth of new
systray items (one top-level item, three checkboxes, one file prompt, one nested "Sync every"
submenu, one action, per integration) have never been clicked through on either target platform.
`make build-windows` is the only CI-reachable compile check that includes this file at all --
`make build-darwin` explicitly excludes both `internal/tray` and `cmd/branchdam-agent`
(`Makefile:47`), so darwin compilation of the menu is unverified anywhere in this repo's own
tooling.

## Status page (issue #61)

The embedded status page's "Integrations" section joins two independent sources at render time,
by ID, via a `integrationView` template func (`text/template` can only call a method that returns
one value, or two where the second is `error` -- `SettingsView.Integration`/`Status.Integration`'s
`(T, bool)` return doesn't qualify, hence the func):

- **Config state** (enabled, dry run, catalog path) from `Settings.Snapshot()` -- `Runner.Status()`
  deliberately never reads config (see `Status.Integrations`'s own doc comment), so `StatusServer`
  now also takes a `SettingsFunc func() SettingsView`, called once per request alongside
  `StatusFunc`. `SettingsFunc` is nil-tolerant (renders an empty `SettingsView`) since several
  existing tests construct a bare `StatusServer{StatusFunc: ...}` literal with no settings source.
- **Runtime state** (last sync time/counts/error) from `Status.Integrations[].LastSync`.

The `(dry run — nothing was emitted)` marker is driven by `SyncSummary.DryRun` -- the flag as it
was **at the time that pass ran** -- not by the config's current dry-run checkbox. Those can
disagree (a pass runs under dry-run, the operator unticks it immediately after); rendering off the
config value instead would make the marker disappear from a stale `Emitted` count that never
actually reached the server, which is the exact "actively misleading" failure mode the issue called
out.

A "DaVinci Resolve render hook" section renders the same cached `HookState` the tray's own hook
installer maintains (`Runner.SetHookState`, seeded once via `resolvehook.Detect` at startup --
never recomputed on the status page's own refresh, for the same reason `statusQueueReadTimeout`
exists for queue counts). **Installed**, **up to date**, and **modified/out of date** are rendered
as distinct states since a hand-edited copy and a stale shipped version are the same SHA-256
mismatch, indistinguishable by design -- see `HookState`'s own doc comment.

**Read-only.** Neither section is itself interactive -- both are plain `<meta http-equiv="refresh">`
HTML, no JS, no POST endpoints (see `handleIndex`'s own doc comment on why this repo never adds one).
The actions themselves live one level up, in the tray menu: catalog sync via the Integrations menu's
"Sync now" (or its background timer), and the Resolve hook via the "DaVinci Resolve" menu's
"Install / update render hook" (issue #68) -- see that section below. `branchdam-agent resolve-hook
-install` remains available headlessly, for a workstation that never runs the tray at all.

## DaVinci Resolve hook menu (issue #68)

A separate top-level menu, sibling to the Integrations menu's own top-level items rather than
nested under it: `internal/tray.HookDescriptors()`'s compile-time registry (DaVinci Resolve's
render hook today) is what `newHooksMenu` builds items from, mirroring `Integrations()`'s own "menu
built once, no rebuild path" constraint.

- **"Install / update render hook"** -- a `Runner` action (`Runner.TriggerHookInstall`), wired
  directly into `run_supported.go`'s own select loop with its own worker goroutine + done channel,
  exactly like "Sync now": `TriggerHookInstall` does a real (small, atomic) file write, which would
  freeze the whole menu including Quit if run inline. Bounded by `hookInstallClickTimeout` (2
  minutes, matching `drainPruneClickTimeout` rather than the much larger
  `integrationSyncClickTimeout` -- a script write is a small, local-or-LAN operation, not a
  third-party catalog read).
- **"Reveal Scripts folder"** -- `Runner.RevealHook`, a fire-and-forget OS shell-out via the
  registered `HookInstaller.Reveal()`. Unlike Install, this needs no done channel or select-loop
  case at all: it mutates no state any submenu's own `sync()` renders, matching "Open status page"'s
  own `_ = openBrowser(statusURL)` precedent of silently discarding the result.
- **No config-driven items** -- a hook has no `CatalogSyncConfig` and no menu-editable
  `integrations.resolve.scriptsDir` override yet (still config-file-only), so `hookSubmenu` has no
  `dispatch()` goroutine and no shared-`menuActionCh` involvement at all, unlike
  `integrationSubmenu`.

The disabled status line's wording is deliberately identical to the status page's own DaVinci
Resolve section (`hookStatusLine` in `hooksmenu.go`), so the menu and the status page never disagree
about what a given `HookState` means.

**Unverified on real hardware**, same caveat as the Integrations menu above: `make build-windows` is
the only CI-reachable compile check that includes this file at all, and the menu items themselves
have never been clicked through on either target platform.

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

### Rollback (issue #33)

Every target `Apply` touches is replaced through its own `go-selfupdate` `Updater` instance,
configured with `OldSavePath: "<target>.previous"` -- go-selfupdate's own `su.Config.OldSavePath`
is a single fixed string per `Updater`, and this package's shared `Updater` is reused across every
target in a layout, so setting it there would make a Windows install's second target (the primary
exe) silently overwrite the sibling exe's just-saved backup at the same path. A fresh per-target
`Updater` is cheap (no network, just a struct) and sidesteps that. Before any target is touched,
the version being replaced is recorded in a sidecar file next to the primary's own backup
(`<primary>.previous.version`) -- written first, deliberately, so a sidecar-write failure can
never orphan an already-successful target's backup with rollback silently unavailable.
`internal/selfupdate.RollbackInfo`/`PreviousVersion` read it to report whether a rollback is
available and to which version, and `Rollback` uses it to restore every target from its own
`.previous` file (via go-selfupdate's exported `update.Apply`, reused in the reverse direction --
never re-validating the backup's checksum, since it was already checksum-validated once, by
`Apply`, at the point it was written) and to rewrite a macOS bundle's `Info.plist` back to the
rolled-back version. Rollback applies to targets in the *same* order `Apply` does -- siblings
first, the running exe last -- for the identical safety reason: a failure on a sibling aborts
before the running binary is touched. Each target's own `.previous` backup is removed immediately
after that target is restored (not batched at the end), and the version sidecar only after a
successful `Info.plist` rewrite -- so a mid-rollback failure leaves exactly the still-pending
state in place (safe to retry: an already-consumed backup is treated as an idempotent no-op, not
an error) rather than either advertising a stale rollback or losing the ability to retry a failed
`Info.plist` write. Once everything succeeds, the tray's "Roll back to vX" menu item (and
`update -rollback`) disappear again until the next `Apply` creates a fresh one.

Rollback makes no network call at all -- it is deliberately **not** gated by `selfUpdate.enabled`,
unlike Check/Apply. An operator who has since disabled self-update checking can still undo an
update they applied while it was enabled. `update -rollback [-yes]` is the headless equivalent of
the tray's "Roll back to vX" menu item; both prompt for confirmation unless `-yes`/an explicit
click bypasses it, mirroring Apply's own confirm-unless-`-yes` shape.

**The per-target `Updater` construction does not fix the pre-existing double download on
Windows.** Each target still calls its own `UpdateTo`, which downloads and checksum-validates the
release archive independently -- twice on Windows, once for each binary, exactly as before this
PR. Genuinely downloading once and applying to N targets would require calling
`go-selfupdate`'s unexported `download`/`validate` methods indirectly (there is no exported
"fetch once, apply many" entry point), which would mean reimplementing this repo's *only*
integrity control (see this section's opening paragraph) as a hand-maintained copy that silently
stops tracking upstream if the configured `Validator` ever changes -- e.g. to a chained
`PatternValidator` for a future PGP-over-SHA256SUMS check. That is a strictly worse trade than one
extra ~10MB download on one platform, so this PR keeps calling `UpdateTo` per target and accepts
the cost. See Known gaps.

### Sigstore attestation

The release workflow (`.github/workflows/release-binaries.yml`) signs every published asset
with `cosign sign-blob --yes` (keyless OIDC mode, pinned to
`https://token.actions.githubusercontent.com` as the OIDC issuer and to this repo's workflow
URL as the certificate SAN). `internal/selfupdate.Apply` runs the matching keyless
verification in-process via `github.com/sigstore/sigstore-go` before
`go-selfupdate`'s `ChecksumValidator` checks the SHA-256. The verify proves the release was
built by this repo's signed release workflow; a release page write by anyone without the
workflow identity cannot produce a signature that passes.

`Apply` runs a `sigstorePreflight` step once per call (between the `GreaterThan` check and
the sidecar-write) that downloads the `.sig` + `.cert` next to the release archive on
GitHub's release-asset CDN (~6 KB total), populating an in-memory cache keyed by asset
name. The per-target `UpdateTo` calls share that cache via a composed
`sigstoreValidator` (SHA-256 first, then Sigstore). The dedupe-by-name in the preflight
ensures the Windows two-binary apply (`branchdam-agent.exe` + `branchdam-agent-tray.exe`
sharing one archive) makes one network round trip per asset, not two. The fetch uses an
`http.Client{Timeout: 30 * time.Second}` so a stalled CDN connection can't block the
preflight indefinitely.

What this does and does not prove:

- **Proves:** the cert chains to a trusted Fulcio root (embedded in the binary, see
  `internal/selfupdate/trusted_root_public_good.json`), the cert's OIDC issuer matches
  `https://token.actions.githubusercontent.com` exactly, the cert's SAN matches
  `^https://github.com/s3ntin3l8/branchdam-agent/`, and the signature is a valid
  ECDSA-P256 over SHA-256 of the archive bytes.
- **Does not prove:** the signing time (Rekor / SCT / TSA verifiers are deliberately
  disabled). Fulcio certs are 10-minute validity windows; without Rekor, a signature
  is "trusted" only as long as the cert was within its 10-minute window at the moment
  of the verify call. This is an acceptable trade for an agent that may be online only
  briefly for self-update: the load-bearing threat the verify closes is "a
  non-maintainer with release-page write access mints a release," which cert+issuer+SAN
  already defeats. Proving the *signing event* (Rekor) requires a workflow change
  (upload the inclusion proof with the asset) and a new online dependency at apply time;
  deferred.

The trusted root is a hand-pruned subset of
`https://github.com/sigstore/sigstore-go/blob/v1.3.0/examples/trusted-root-public-good.json`
(Fulcio root + intermediate only; Rekor / CT / TSA sections are removed because the
verify path skips those verifiers). When Sigstore rotates the Fulcio root, future
releases will fail cert-chain verification against the stale embedded root. **At that
point a future PR must re-fetch the upstream example, prune the same way, and re-commit
`internal/selfupdate/trusted_root_public_good.json` -- otherwise every workstation
running the agent will refuse to self-update.** Track
`https://github.com/sigstore/Fulcio` for rotation announcements; see
`internal/selfupdate/sigstore.go`'s package doc for the rotation cadence rationale.
A live-refresh via TUF is the proper long-term answer but is out of scope here.

## Known gaps

- **`LSUIElement=1` Dock-suppression is unverified on real hardware**, as is whether AppKit's
  bundle discovery still honors it when launchd execs the bundle's inner binary directly rather
  than opening the `.app` -- see the macOS `.app` bundle section above.
- **The exact Gatekeeper prompt wording, and whether App Translocation actually triggers the way
  described above, are unverified** without a macOS host to observe them directly.
- **The Windows two-binary self-update apply, the macOS relaunch-via-`open -n -a` path, and
  rollback (issue #33) are all unverified end-to-end** -- `internal/selfupdate`'s archive-entry-
  selection, checksum, and rollback-restore logic are pinned by tests that run on Linux CI (see
  that package's tests, including `rollback_test.go` against fabricated `InstallLayout`s), but a
  full download-verify-swap-relaunch cycle, and a subsequent roll-back, have not been exercised on
  real Windows or macOS hardware. See the Hardware verification checklist below.
- **The release archive is still downloaded and checksum-validated once per target -- twice on
  Windows -- even after issue #33's rollback work.** Accepted cost, not fixed: see the Rollback
  subsection above for why a genuine single-download fix would mean reimplementing
  `go-selfupdate`'s unexported download/validate pipeline by hand, moving this repo's only
  integrity control out of the library's hands.
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
- **Tray queue status has no live rate/ETA or in-progress-transfer readout (issue #32, closed
  by #85 for ingest; drain ETA still pending).** The status page and menu show a real
  `internal/queue.Store.Counts` snapshot (pending, permanently failed, done) plus the last
  completed drain/prune pass's summary. Live per-file ingest progress (path, bytes done / total,
  phase, elapsed time) is shipped by #85: `internal/ingest/progress.go`'s `ProgressEvent` is
  emitted by `DualWrite`, `WriteLocal`, `Verify`, `CopyToArchive`, and `Drain`'s archive copy,
  rendered in the tray tooltip via `internal/tray/tooltip.go`'s `FormatIngestProgress` and on
  the status page (`TestHandleIndexRendersIngestProgress`,
  `TestRunnerIngestProgressRecordedAndCleared`). What remains open: a bytes/sec rate / ETA for
  drain's archive copy (the bytes/sec signal exists per-file but is not yet aggregated across
  the concurrent copy + verify pipeline). `offline.drainIntervalSecs`/`prune.intervalMinutes`
  are also config-file-only for now -- not yet exposed in the tray's settings menu. Note also
  that `Runner.Status()` performs a real `queue.Store.Counts` read on every call -- the tray's
  5s menu-refresh tick and every status-page request -- bounded to 5s (`statusQueueReadTimeout`)
  so a wedged read can't hang the whole tray menu, but still a real query against `queue.db`
  each time; this is more impactful the larger or more NAS-backed-network-latency-prone
  `offline.queueDbPath`'s storage is.
- **Tray-driven drain/prune passes are unverified on real hardware.** `Runner.TriggerDrain`/
  `TriggerPrune`'s locking (a dedicated mutex for drain, sharing the ingest/self-update gate for
  prune) and the two background timers (`cmd/branchdam-agent/queueagent.go`'s `startPeriodic`) are
  pinned by unit tests with fake `Drainer`/`Pruner`s, but nothing here has exercised a real
  drain/prune pass running concurrently with a real card ingest on Windows or macOS.
- **The Luminar Neo catalog query is verified against exactly one catalog version.** Checked
  against `db_version 155` (see [`luminar-catalog.md`](luminar-catalog.md) for the full record);
  a different Luminar Neo version's schema is unconfirmed. More importantly, Luminar Neo itself
  stores no relational edit->source lineage at all -- pairing is inferred from filename
  convention (`_upscale`/`_panorama`), not read from the catalog. Use `--dump-schema` /
  `-query-file` to correct row extraction against a different version, and
  `-derivative-suffixes` to correct the pairing heuristic without a code change.
- **`prune` is not real Tier-1 NLE scratch pruning.** It only ever considers files ingested via
  `ingest -offline` (rows in `queue.db`) -- a plain online `ingest` has no durable local-path
  ledger to prune against. Real Tier-1 `LOCAL_SCRATCH` pruning stays architecturally blocked on
  branchDAM's side (branchDAM issues #230, #266): nothing can mint a `storage_location_id` for
  an unmounted tier without breaking that invariant.

## Hardware verification checklist

Everything above marked "unverified on real hardware" needs a real Windows 11 machine and a real
macOS (Apple Silicon) machine to check off -- nothing in this list can be exercised from this
repo's Linux-only development/CI environment. Run through this after any change to
`internal/tray`, `internal/selfupdate`, or `internal/appbundle`; check off what passes, and file
an issue (with what actually happened) for what doesn't, rather than silently updating this list
to say "verified."

**Windows:**

1. First-run bootstrap: delete `%AppData%\branchdam-agent\config.yaml`, launch
   `branchdam-agent-tray.exe` directly (not via a console) -- confirm the setup wizard's dialogs
   (server URL, API key, archive/local roots, path mapping) render as native Win32 dialogs, not a
   silent failure, and that a starter config is left on disk if you cancel partway.
2. Startup-error dialog: hand-edit `config.yaml` to something `Validate()` rejects (e.g. an
   unexpanded `${VAR}` in `server.apiKey`), launch `branchdam-agent-tray.exe` -- confirm an error
   dialog appears (before systray's own message pump has started) naming the log path at
   `%LOCALAPPDATA%\branchDAM\logs\agent.log`.
3. Settings menu dialogs: with a config already in place, open the tray's Settings submenu and
   trigger each free-text prompt (server URL, API key, naming template, the two folder pickers) --
   confirm each renders correctly while systray's own message pump is *already running* (a
   materially different scenario from item 2's pre-`systray.Run` dialog).
4. Full self-update cycle: publish (or point `selfUpdate.repo` at) a test release one patch version
   above the running build, click "Install and restart" -- confirm both `branchdam-agent.exe` and
   `branchdam-agent-tray.exe` end up at the new version, the tray relaunches cleanly, and
   `%LOCALAPPDATA%\branchDAM\logs\agent.log` shows no partial-apply errors.
5. Rollback: immediately after step 4, click "Roll back to vX" (or run
   `branchdam-agent update -rollback`) -- confirm both `.exe`s are restored to the pre-update
   version, the tray relaunches, and a second "Roll back" attempt correctly reports nothing left
   to roll back to.
6. Tray-driven queue: with `offline.queueDbPath` and `prune.enabled: true` set, ingest a card
   offline, then let the tray's own drain/prune timers run (or use "Drain queue now"/"Prune now")
   -- confirm the status page's queue counts update and a card ingest started while a drain/prune
   pass is in flight is never blocked or corrupted.
7. Integrations menu: open the top-level "Luminar Neo" item (a sibling of Settings, not nested
   under it) -- confirm the "Catalog…" and "Node index…" file pickers render (`-kind file` in the
   `dialog` subcommand) while systray's message pump is already running, that ticking "Enabled"
   with no catalog path set yet shows the "not fully configured" status line rather than a crash or
   a silent no-op, and that toggling "Dry run" off and clicking "Sync now" against a real catalog
   actually reaches the configured `server.baseUrl` (check the audit queue for the emitted edge --
   confirm the status page's own Integrations section shows the same emitted count with no
   "(dry run — nothing was emitted)" marker once it's off). Also confirm a Settings-menu click and
   an Integrations-menu click issued back-to-back never cross-contaminate each other's "last change
   failed" title.
8. Timer-driven sync: set `integrations.luminar.syncIntervalMinutes: 1` and leave the tray running
   with "Dry run" on -- confirm a sync pass fires on its own (no menu click) roughly once a minute,
   the status page's "last sync" timestamp advances each time, and the `(dry run)` marker is present
   throughout since no real POST should ever leave the workstation.
9. Resolve hook install, headless: run `branchdam-agent resolve-hook -install` with no `-dir`
   override -- confirm it installs into
   `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\Fusion\Scripts\Utility\`, and opening the
   script from Resolve's own Workspace ▸ Scripts ▸ Utility menu actually runs it.
10. DaVinci Resolve hook menu (issue #68): open the top-level "DaVinci Resolve" item (a sibling of
    the Integrations menu's own items) -- confirm the disabled status line matches whatever item 9
    just installed, click "Install / update render hook" and confirm the status line updates to
    "installed and up to date" without a tray restart (state is cached and refreshed only by this
    click -- see the DaVinci Resolve hook menu section above), and click "Reveal Scripts folder" and
    confirm Explorer opens the correct directory. Also confirm a rapid double-click on "Install /
    update render hook" shows the "(skipped just now -- already running)" note rather than running
    two installs concurrently or silently dropping the second click.

**macOS (Apple Silicon):**

1. `LSUIElement=1` Dock suppression: launch `branchdam-agent.app` from `/Applications` -- confirm
   no Dock tile and no Cmd-Tab entry appear, both when opened normally (`open -a`) and via a
   LaunchAgent (`tray.startOnLogin: true`, which execs the bundle's inner binary rather than
   opening the bundle -- confirm AppKit's Dock-suppression still applies in that path too).
2. Gatekeeper/quarantine: download the release archive via a browser (not `curl`), extract, and
   launch `branchdam-agent.app` from `~/Downloads` without moving it first -- confirm it either
   triggers App Translocation (and `internal/selfupdate.DetectLayout` refuses with
   `ErrTranslocated`, surfaced as a startup-error dialog, not a crash) or exhibits whatever the
   actual Gatekeeper prompt wording turns out to be. Then move it to `/Applications` and confirm
   translocation clears.
3. Startup-error dialog and settings dialogs: same two checks as Windows items 2-3 above, but
   confirm the `osascript`-backed dialog renders correctly when the bundle is launched by launchd
   (a LaunchAgent `RunAtLoad`), not just from Finder.
4. Full self-update cycle: same as Windows item 4 -- confirm go-selfupdate replaces only
   `branchdam-agent.app/Contents/MacOS/branchdam-agent` (never the bundle itself), `Info.plist`'s
   `CFBundleVersion` is rewritten to match, and the relaunch (`open -n -a`, not a bare `exec`)
   produces exactly one running tray process, not two.
5. Rollback: same as Windows item 5 -- additionally confirm `Info.plist`'s `CFBundleVersion` is
   rewritten back to the rolled-back version, not left at the version being rolled back FROM.
6. Tray-driven queue: same as Windows item 6.
7. Integrations menu: same as Windows item 7 -- confirm the `-kind file` picker renders correctly
   via `osascript`, including when the tray is launched by launchd rather than from Finder.
8. Timer-driven sync: same as Windows item 8.
9. Resolve hook install, admin-rights path (headless): `branchdam-agent resolve-hook -install` with
   no `-dir` override targets the per-user
   `~/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility/` path
   first (no admin rights needed) -- confirm that succeeds, then separately confirm the
   ADMIN-RIGHTS path by running
   `branchdam-agent resolve-hook -install -dir "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility"`
   without `sudo` and confirming it fails with a permissions error (not a silent no-op), then with
   `sudo` and confirming it succeeds. Confirm Resolve picks up the script from whichever location it
   ends up in via Workspace ▸ Scripts ▸ Utility.
10. DaVinci Resolve hook menu: same as Windows item 10 -- additionally confirm "Reveal Scripts
    folder" opens the per-user path via `osascript`/Finder, including when the tray is launched by
    launchd rather than from Finder.

### M5 additions (epic #77, #78–#88)

The M5 work landed several new tray behaviors whose code paths are covered by
unit tests but whose rendering / OS-integration paths have never been exercised
on real hardware. Add these checks to the Windows and macOS lists above as
appropriate:

11. **Settings menu — M5 dialogs (#78, #79, #81):** with a config already in
    place, open the tray's Settings submenu and trigger each of the new M5
    items -- "Watch folders…" (comma-separated text entry for `cardRoots`; the
    dialog kind is "entry", not a native directory picker, because
    `cardRoots` is a multi-value list, not a single folder), "Allowed
    extensions…" (comma-separated text for the extension allow-list), and the
    autoImportPaths list-editing dialog (reached via the card-detection
    confirmation flow's "Always auto-import"). Confirm each dialog renders
    correctly while systray's own message pump is already running (the
    materially-different scenario from item 2's pre-`systray.Run` startup
    dialog). Also: trigger a card detection with the dialog path, click
    "Skip this time" -- confirm a re-insert re-shows the dialog; click
    "Always auto-import" -- confirm the volume is now in the allow-list and
    re-insert bypasses the dialog.
12. **Settings menu — M5 native checkboxes (#81, #83, #84, #87):** with the
    same config in place, toggle "Require DCIM folder" (insert a USB stick
    with no DCIM folder -- confirm not detected), "Auto-eject after ingest"
    (insert a card -- confirm a successful ingest unmounts/ejects the volume
    and surfaces an OS notification; induce an ingest error -- confirm the
    card is NOT ejected on partial ingest), "Pause upload on metered
    connection" (toggle on, then on a real hotspot/iPhone-tethered network
    confirm queue drain is skipped and the local edit copy still proceeds),
    and "Pause ingest" (toggle on, insert a card -- confirm no ingest starts
    AND no dialog appears; toggle off, confirm a re-insert triggers the
    dialog again).
13. **Live ingest progress in tooltip + status page (#85):** insert a card,
    open the tray menu -- confirm the tooltip updates within one tick with
    the per-file line rendered by `FormatIngestProgress` (filename, bytes
    done / total, percentage, speed, ETA); the status page renders the same
    data plus the `phase` field under the busy-card header. Confirm the
    tooltip clears (reverts to the default "branchDAM agent" string) within
    one tick of ingest completion.
14. **Pre-flight BLAKE3 dedup (#88):** ingest a card. Then re-insert the same
    card. Confirm the second ingest reports every file as
    `duplicate: already in library as node X` with zero bytes written to
    `archiveRoot`. Then disconnect from the server (simulate offline: pull
    the network cable / disable Wi-Fi), re-insert the card -- confirm the
    pre-flight times out within 5s (`PreflightTimeoutSecs`), the ingest
    proceeds normally, and the server-side dedup fires on the next
    `PostNodeCreated` once connectivity is restored.
15. **Safe eject after verified ingest (#87):** with `autoEject: true` set,
    insert a camera card -- confirm the volume unmounts/ejects after the
    verified ingest completes and an OS notification appears. On Windows
    specifically, confirm the tray surfaces `IOCTL_STORAGE_EJECT_MEDIA` via
    `DeviceIoControl` cleanly (no "device in use" dialog from the OS -- the
    card must have been closed by `Verify`'s unbuffered re-open before the
    eject call). On macOS, confirm `diskutil unmountDisk` reports `Volume X
    on diskY unmounted` and the card disappears from Finder. On Linux,
    confirm the udisks2 path runs (`udisksctl unmount -b <dev>` then
    `udisksctl power-off -b <dev>`, with the device resolved from
    `/proc/mounts` -- no direct udev interaction), visible in
    `%XDG_STATE_HOME%/branchdam-agent/agent.log`, and the card's mount point
    is gone from `mount`.

All M5 items above are also pinned by tests that run on Linux CI --
`internal/ingest/offline_test.go::TestIngestFileOfflineDedupTimeout` for
#88's offline timeout fall-open, `internal/tray/tray_test.go::TestAutoEject*`
for #87, `internal/tray/tray_test.go::TestTriggerIngestSkipsWhenPaused` (1893),
`TestTriggerDrainSkipsWhenPaused` (1915), and `TestTriggerDrainPauseOnMetered`
(1943) for #83 / #84's gate logic, `internal/ingest/progress_test.go::Test*Progress`
for #85, and the unit tests called out in AGENTS.md invariants #15–#17 for
the rest (added in PR-B #165). What this checklist adds is the real-OS-integration
half that no test on Linux CI can substitute for.
