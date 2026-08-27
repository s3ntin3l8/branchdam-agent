# Platform support

The headless subcommands (`preflight`, `ingest`, `queue-drain`, `luminar-sync`, `prune`) run on
Windows, macOS, and Linux. The tray shell (`tray`) is Windows/macOS only.

## Support matrix

| | Windows | macOS (Apple Silicon) | Linux |
|---|---|---|---|
| Headless subcommands | yes | yes | yes |
| Tray shell | yes | yes | no -- `tray: unsupported on this platform` |
| Release binary | `branchdam-agent.exe` + `branchdam-agent-tray.exe` | `branchdam-agent` (arm64 only, no amd64) | `branchdam-agent` |

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

## Self-update

Gated only by `selfUpdate.enabled: true` in config -- no build tag required, compiled into
every build. `go-selfupdate` v1.6.0 imports `golang.org/x/crypto/openpgp` unconditionally from
its top-level package, which `govulncheck` flags as `GO-2026-5932` (unfixed, no upgrade path --
v1.6.0 is the newest tag). The `test-go / lint-and-test` check suppresses that specific finding
via `s3ntin3l8/.github/ci-go.yml`'s `govulncheck-ignore` input. See tracked issue #14 for when
the ignore entry can be dropped.

## Known gaps

- **macOS `.app` bundle not implemented.** The tray currently ships as a bare binary in a
  tarball (`release-binaries.yml`'s `build-darwin` job). The stack-decision plan for this repo
  calls a hand-assembled `.app` bundle with `Info.plist`/`LSUIElement=1` non-optional for a
  tray app, to avoid an unwanted Dock icon. **Dock-icon behavior is unverified** -- no macOS
  host has been used interactively to check it. Verify this on real hardware before relying on
  the macOS tray for day-to-day use.
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
