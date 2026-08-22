# branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) (phase 10 of the
original spec) -- a Go binary that will eventually ingest SD cards, keep an offline queue, and
report to branchDAM over its existing `/api/v1/agent/*` REST contract. See branchDAM's
`.claude/plans/can-we-walk-through-sharded-lighthouse.md` for the full phased plan this repo
implements.

Landed so far: **M0** (repo scaffold + REST client), **M1** (SD-card ingest core, dual-copy
verified write, plus the tray shell around it), **M2** (offline queue + rebase handoff), **M3**
(DaVinci Resolve post-render hook, `hooks/resolve/`), and **M4** (Luminar `catalog.db` reader).
See [CLAUDE.md](CLAUDE.md) for the milestone breakdown.

## What's here today

- `internal/branchdam/` -- the REST client for branchDAM's agent-server contract
  (`hello`/`handshake`/`events`/`rebase`), with DTOs hand-synced to branchDAM's own
  `internal/agent/types.go` and `internal/httpapi/routes.go`, plus a golden-file conformance test
  (`internal/branchdam/conformance_test.go`).
- `internal/hashing/`, `internal/naming/`, `internal/phash/` -- ports of the three pieces of
  branchDAM server logic an agent-ingested file must reproduce exactly to stay consistent with a
  normal server-side scan (`FastHash`'s sampled-window algorithm, `naming.Stem`'s filename
  normalization, and `ExtractPHash`'s decode-then-exiftool-fallback call sequence), each with
  golden-vector tests generated from branchDAM's real implementation.
- `cmd/branchdam-agent/` -- a `preflight` subcommand: checks the configured branchDAM server is
  reachable and returns its version, checks `exiftool` on `PATH`, and prints the configured
  workstation-path -> container-path mappings.
- `internal/ingest/` -- the SD-card ingest core: poll-based card detection, one-read/two-write
  dual-copy writer, a cache-defeating verified re-read (`fsync`+close+reopen floor,
  unbuffered/`O_DIRECT` where the platform supports it), DJI `.srt` telemetry parsing for the
  video's own GPS fields, and metadata extraction at promoted-column parity with a server-side
  scan. No UI imports -- the headless `ingest -card <path>` subcommand and the `tray` subcommand
  below both drive it, neither duplicates it.
- `internal/queue/`, `internal/ingest`'s `IngestCardOffline`/`Drain` -- M2's offline queue
  (`ingest -offline`, `queue-drain`): every intended event persisted to `queue.db`
  (`modernc.org/sqlite`) before any network call, so a workstation with no route to the NAS can
  still ingest a card, then finish the archive copy and `POST /api/v1/agent/rebase` once
  reconnected -- see [`docs/offline-queue.md`](docs/offline-queue.md) for the full state machine,
  the copy-before-rebase ordering guarantee, and the server-side prerequisite this depends on.
- `internal/luminar/`, `internal/nodeindex/` -- a `luminar-sync` subcommand: reads a Luminar
  `catalog.db` read-only (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` at
  `tier: 2, confidence: 0.89` for each edit->source pair it finds and can resolve to known
  `nodeUuid`s. Luminar's schema is undocumented -- see
  [`docs/luminar-catalog.md`](docs/luminar-catalog.md) for the research, confidence level, and how
  to correct the query against a real catalog.
- `internal/tray/`, `internal/autostart/`, `internal/selfupdate/` -- the tray shell (issue #3): a
  `fyne.io/systray` icon/menu (windows/darwin only) plus an embedded `net/http` status page
  showing watch directories, scratch-directory info, and queue status (a stub -- M2's offline
  queue landed concurrently with this PR and isn't wired into the status page's display yet);
  login-item registration (off by default); `go-selfupdate` wiring (off by default via config, and
  compiled in only with `-tags selfupdate` -- see "Tray shell" below for platform-specific
  findings from this PR).

## Quick Start

### 1. Installation

```sh
make install-hooks   # set up pre-commit + pre-push hooks
```

### 2. Development

```sh
make build           # compile all packages
make test            # run tests with race detection
```

### 3. Run preflight against a branchDAM server

```sh
cp config.example.yaml config.yaml
# edit config.yaml: server.baseUrl, server.apiKey (>= 32 chars), agentId, pathMappings
go run ./cmd/branchdam-agent preflight -config config.yaml
```

### 4. Ingest an SD card, including offline

```sh
# Online (server reachable): dual-copy write, immediate EVENT_NODE_CREATED.
go run ./cmd/branchdam-agent ingest -config config.yaml -card /media/$USER/UNTITLED

# Offline (no route to the NAS/server): local copy only, everything else queued.
go run ./cmd/branchdam-agent ingest -config config.yaml -card /media/$USER/UNTITLED -offline

# On reconnect: submit queued events, copy archive bytes, rebase to Tier-3.
go run ./cmd/branchdam-agent queue-drain -config config.yaml
# Or keep draining until connectivity returns:
go run ./cmd/branchdam-agent queue-drain -config config.yaml -watch
```

`-offline` requires `offline.queueDbPath` and `offline.tier0ContainerRoot` set in `config.yaml`
(see `config.example.yaml`), and branchDAM must have a matching `TIER0_LOCAL_STAGING` storage
location configured -- see [`docs/offline-queue.md`](docs/offline-queue.md) before relying on this
against a real deployment.

### 5. Sync a Luminar catalog

```sh
# Dry run first -- resolves and logs what would be emitted, never contacts the server:
go run ./cmd/branchdam-agent luminar-sync -catalog /path/to/catalog.db -node-index node-index.json -dry-run

# Recover a real catalog's actual schema (see docs/luminar-catalog.md before trusting the
# built-in query against your own catalog):
go run ./cmd/branchdam-agent luminar-sync -catalog /path/to/catalog.db -dump-schema

go run ./cmd/branchdam-agent luminar-sync -config config.yaml -catalog /path/to/catalog.db -node-index node-index.json
```

`node-index.json` maps absolute file paths to the `nodeUuid`s they were ingested as -- see
[`internal/nodeindex`](internal/nodeindex/nodeindex.go)'s doc comment for why this exists (no
agent-reachable lookup-by-path endpoint on branchDAM yet) and
[`docs/luminar-catalog.md`](docs/luminar-catalog.md) for the node-resolution scope decision.

### 6. Run the tray shell

```sh
go run ./cmd/branchdam-agent tray -config config.yaml
```

Starts the tray icon (windows/darwin) plus an embedded status page (default
`http://127.0.0.1:38080/`, loopback-only -- see `tray.statusAddr` in `config.example.yaml`)
showing configured watch directories, the local scratch directory, and queue status (a stub until
M2's offline queue lands -- deliberately labelled as such, never a fabricated number). The tray
menu's "Ingest now" and its automatic card-insertion trigger both call the same
`internal/ingest.Engine.IngestCard` the headless `ingest` subcommand uses.

On Linux, `tray` builds and runs, but immediately returns an error (`tray: unsupported on this
platform`) -- the tray is scoped to Windows/macOS per the plan doc; a Linux workstation still has
the fully-tested headless `ingest` path.

## Tray shell: platform-specific findings (issue #3)

This PR could only be developed and CI-verified from a Linux host. What was checked, and how:

- **Windows console flash.** `-H windowsgui` is link-scoped to the *whole* binary, and this one
  binary also serves `preflight`/`ingest`/`luminar-sync`, whose stdout an operator needs in a
  console. So there are two Windows build outputs from the same source
  (`make build-windows`): `branchdam-agent.exe` (console-linked, for the CLI subcommands) and
  `branchdam-agent-tray.exe` (`-H windowsgui`-linked, for the tray/login-item launch path).
  Verified locally: `file dist/branchdam-agent-tray.exe` reports `PE32+ executable (GUI)` versus
  `(console)` for the other binary.
- **`fyne.io/systray` v1.12.2's cross-compile matrix**, verified with a throwaway probe before
  writing any tray code: pure Go (`CGO_ENABLED=0`) on `GOOS=windows`; needs cgo (Objective-C) on
  `GOOS=darwin`, which fails to even compile with `CGO_ENABLED=0` (`undefined: setInternalLoop`
  etc, not a linker error) and there is no darwin cgo cross-toolchain on `ubuntu-latest`. This is
  why `internal/tray` isolates the systray import behind a `windows || darwin` build tag
  (`run_supported.go`) with a `run_unsupported.go` stub for every other `GOOS` (including Linux,
  where CI actually builds/tests/lints this repo) -- and why the darwin CI leg
  (`build-darwin` in `.github/workflows/ci-cd.yml`) is a build-only check over everything
  **except** `internal/tray` and `cmd/branchdam-agent` (which imports it): that's the boundary
  Linux CI can actually prove for darwin/arm64, not the whole tray binary.
- **macOS Dock-icon behavior: still UNVERIFIED.** No macOS host was available (`uname -a` on the
  development machine: Linux). If a bare `fyne.io/systray` binary shows a Dock icon on macOS, the
  fix is a hand-assembled `.app` bundle with `Info.plist`/`LSUIElement=1` -- **not implemented in
  this PR**. Check this first on real hardware before shipping a macOS build to an actual
  workstation.
- **`macos-26` GitHub-hosted runner availability for this public repo: see this PR's description**
  for what the `build-darwin-full` job (`.github/workflows/ci-cd.yml`) actually did on the first
  push -- it is not a required branch-protection check, so a queued/unavailable outcome there
  does not block merging, but it does determine whether the darwin *tray* binary (as opposed to
  the build-only check above) gets proven in CI at all.
- **Login-item registration** (`internal/autostart/`) is implemented for both platforms --
  `autostart_darwin.go` writes/loads a LaunchAgent plist, `autostart_windows.go` writes an
  `HKCU\...\Run` value via `golang.org/x/sys/windows/registry` -- and is testable up to the actual
  file/registry write: the plist XML rendering (`autostart.go`, untagged) has unit tests that run
  on Linux; the platform-tagged write paths are proven only by the `build-windows`/`build-darwin`
  cross-compiles, same caveat as the rest of this list. Off by default (`tray.startOnLogin` in
  config).

## Commands

| Command | Does |
|---------|------|
| `make install-hooks` | Install pre-commit + pre-push hooks. |
| `make test` | Run Go tests with race detection and coverage. |
| `make lint` | Run pre-commit on all files. |
| `make fmt` | Format Go code. |
| `make vet` | Run go vet. |
| `make tidy` | Run go mod tidy. |
| `make vulncheck` | Check for known vulnerabilities. |
| `make build` | Build all packages (host OS/arch). |
| `make build-windows` | Cross-compile both Windows binaries into `dist/` -- see "Tray shell" above. |
| `make build-darwin` | Build-only check for darwin/arm64, excluding `internal/tray`/`cmd/branchdam-agent` -- see "Tray shell" above. |
| `make clean` | Remove build artifacts and caches. |

## Security

- This project follows the [s3ntin3l8 Global Security Policy](https://github.com/s3ntin3l8/.github/blob/main/SECURITY.md).
- Security scans (CodeQL) and dependency reviews are automated in the CI pipeline.
- `detect-secrets` runs in pre-commit and CI against `.secrets.baseline`.

## Workstation hooks

`hooks/` holds standalone scripts that run on a workstation outside this repo's Go
service, for tools that don't have their own agent client. See
[`hooks/resolve/README.md`](hooks/resolve/README.md) for the DaVinci Resolve
post-render `.dam.json` hook.

## Releases

Releases are automated via [Release Please](https://github.com/googleapis/release-please).
Use [Conventional Commits](https://www.conventionalcommits.org/) to trigger version bumps. No
Docker image is published -- this is a desktop CLI/tray binary, not a service.

## License

AGPL-3.0
