# branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) (phase 10 of the
original spec) -- a Go binary that ingests SD cards, keeps an offline queue, and reports to
branchDAM over its `/api/v1/agent/*` REST contract. See branchDAM's
[`docs/roadmap.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/roadmap.md) for how
this repo fits into the overall project, and
[`docs/agent-protocol.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/agent-protocol.md)
for the wire contract this repo implements against.

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
  unbuffered/`O_DIRECT` where the platform supports it), verify-failure cleanup of partial files,
  collision resolution with auto-suffixing (`_2`, `_3`) and identical-content skip, DJI `.srt`
  telemetry parsing for the video's own GPS fields, and metadata extraction at promoted-column
  parity with a server-side scan. No UI imports -- the headless `ingest -card <path>` subcommand and
  the `tray` subcommand below both drive it, neither duplicates it.
- `internal/queue/`, `internal/ingest`'s `IngestCardOffline`/`Drain` -- the offline queue
  (`ingest -offline`, `queue-drain`): every intended event persisted to `queue.db`
  (`modernc.org/sqlite`) before any network call, so a workstation with no route to the NAS can
  still ingest a card, then finish the archive copy and `POST /api/v1/agent/rebase` once
  reconnected -- see [`docs/offline-queue.md`](docs/offline-queue.md) for the full state machine,
  the copy-before-rebase ordering guarantee, and the server-side prerequisite this depends on.
- A `prune` subcommand -- not the same thing as real Tier-1 NLE scratch pruning, which stays
  architecturally blocked (see `internal/config.PruneConfig`'s doc comment and
  [`docs/platform-support.md`](docs/platform-support.md#known-gaps)): deletes an
  offline-ingested file's `ingest.localEditRoot` mirror once
  `POST /api/v1/agent/node-status` (branchDAM's first agent-reachable read endpoint) confirms the
  Tier-3 archive copy is live and hash-verified. Only ever considers `queue.db` rows -- a plain
  online `ingest` has no durable local-path ledger to prune against. Two independent safety
  checks run before any deletion: a symlink-aware containment check against `LocalEditRoot`, and
  a size/mtime re-stat against what was recorded at ingest time.
- `internal/luminar/`, `internal/nodeindex/` -- a `luminar-sync` subcommand: reads a Luminar
  `catalog.db` read-only (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` at
  `tier: 2, confidence: 0.89` for each edit->source pair it finds and can resolve to known
  `nodeUuid`s. Luminar's schema is undocumented -- see
  [`docs/luminar-catalog.md`](docs/luminar-catalog.md) for the research, confidence level, and how
  to correct the query against a real catalog.
- `internal/tray/`, `internal/autostart/`, `internal/selfupdate/` -- the tray shell: a
  `fyne.io/systray` icon/menu (windows/darwin only) plus an embedded `net/http` status page
  showing watch directories, scratch-directory info, and queue status; login-item registration
  (off by default); `go-selfupdate` wiring (off by default via config) -- see
  [`docs/platform-support.md`](docs/platform-support.md) for the per-platform details and known
  gaps (including the queue-status stub).

## Install

Download the archive for your platform from the
[latest release](https://github.com/s3ntin3l8/branchdam-agent/releases/latest) and verify it
against the release's `SHA256SUMS.txt`:

| Platform | Asset | Contains |
|---|---|---|
| Linux (amd64) | `branchdam-agent-linux-amd64.tar.gz` | `branchdam-agent` -- headless subcommands only, no tray |
| Windows (amd64) | `branchdam-agent-windows-amd64.zip` | `branchdam-agent.exe` (console, for CLI use) + `branchdam-agent-tray.exe` (no console, for the tray/login-item launch path) |
| macOS (Apple Silicon) | `branchdam-agent-darwin-arm64.tar.gz` | `branchdam-agent` -- includes the tray |

```sh
tar -xzf branchdam-agent-<platform>.tar.gz    # linux/darwin
sha256sum -c SHA256SUMS.txt                    # verify
```

Binaries are unsigned -- see "Releases" below. See
[`docs/platform-support.md`](docs/platform-support.md) for the full support matrix, including
why Windows ships two `.exe`s and what's not yet implemented per platform.

## Quick Start

### 1. Development setup

```sh
make install-hooks   # set up pre-commit + pre-push hooks
```

### 2. Build from source

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

### 6. Prune already-archived local-edit mirrors

```sh
# Preview only -- never deletes anything:
go run ./cmd/branchdam-agent prune -config config.yaml -dry-run

go run ./cmd/branchdam-agent prune -config config.yaml
# Or keep pruning periodically:
go run ./cmd/branchdam-agent prune -config config.yaml -watch
```

Requires `prune.enabled: true` in `config.yaml` (defaults to false -- opt-in, on purpose) and
the same `offline.queueDbPath`/`ingest.localEditRoot` used by `-offline` ingest and
`queue-drain` above. Only files ingested via `ingest -offline` are ever candidates; a plain
online `ingest` run has nothing for this to check against.

### 7. Run the tray shell

```sh
go run ./cmd/branchdam-agent tray -config config.yaml
```

Starts the tray icon (windows/darwin) plus an embedded status page (default
`http://127.0.0.1:38080/`, loopback-only -- see `tray.statusAddr` in `config.example.yaml`)
showing configured watch directories, the local scratch directory, and queue status. The tray
menu's "Ingest now" and its automatic card-insertion trigger both call the same
`internal/ingest.Engine.IngestCard` the headless `ingest` subcommand uses.

On Linux, `tray` builds and runs, but immediately returns an error (`tray: unsupported on this
platform`) -- the tray is scoped to Windows/macOS; a Linux workstation still has the fully-tested
headless `ingest` path.

Self-update support (off by default -- see `selfUpdate.enabled`) is compiled into every build; no
build tag is required. See [`docs/platform-support.md`](docs/platform-support.md) for the full
per-platform breakdown (why Windows ships two `.exe`s, the `fyne.io/systray` cross-compile
matrix, login-item registration, and known gaps like the unverified macOS Dock-icon behavior and
the queue-status stub).

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
| `make build-windows` | Cross-compile both Windows binaries into `dist/` -- see [`docs/platform-support.md`](docs/platform-support.md). |
| `make build-darwin` | Build-only check for darwin/arm64, excluding `internal/tray`/`cmd/branchdam-agent` -- see [`docs/platform-support.md`](docs/platform-support.md). |
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

Releases are cut automatically via [Release Please](https://github.com/googleapis/release-please)
-- use [Conventional Commits](https://www.conventionalcommits.org/) to trigger version bumps.
When release-please creates a GitHub Release, a chained CI job
(`.github/workflows/release-please.yml` -> `release-binaries.yml`) cross-compiles and attaches
per-platform archives plus a `SHA256SUMS.txt`, with no manual step. No Docker image is
published -- this is a desktop CLI/tray binary, not a service.

**Binaries are unsigned.** No code-signing certificate is purchased for either platform; expect
a Gatekeeper/SmartScreen warning on first run.

## License

AGPL-3.0
