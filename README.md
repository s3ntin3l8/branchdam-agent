# branchdam-agent

The workstation companion agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) — a cross-platform desktop application and CLI that handles SD card media ingestion, dual-copy verified storage writes, offline queueing, and catalog synchronization.

---

## Core Capabilities

- **Bit-for-Bit Verified Ingest**: Dual-write engine reads camera SD cards once and streams simultaneously to your NAS archive (Tier 3) and fast local NVMe edit drive (Tier 1), computing BLAKE3-256 and xxHash64 checksums in flight. Flushes and performs cache-busting unbuffered re-reads (`O_DIRECT` / `F_NOCACHE`) to guarantee data integrity.
- **Offline Field Ingest**: Ingest media on the go without network access to your NAS or server. Events and local files are safely recorded in a local SQLite queue (`queue.db`), then automatically copied to the archive and rebased upon reconnecting to your LAN.
- **System Tray & GUI (Windows & macOS)**: Lightweight menu bar / system tray companion providing auto card-detection, real-time queue status, and a native Settings UI for paths and server credentials.
- **Catalog & NLE Synchronization**:
  - **Skylum Luminar Neo**: `luminar-sync` reads a local `.luminarneo` catalog and infers derived-file->source links from filename convention.
  - **DaVinci Resolve**: Post-render Python hook (`hooks/resolve/`) writes `.dam.json` sidecars for confidence-1.00 timeline-to-export lineage.
  - **Local Mirror Pruning**: `prune` safely reclaims local edit scratch space once branchDAM confirms the master archive copy is hash-verified.

---

## Architecture & Wire Protocol

The agent communicates with the branchDAM server via its `/api/v1/agent/*` REST API contract with static `X-API-Key` authentication. See:
- [`docs/offline-queue.md`](docs/offline-queue.md) — Offline queue state machine and crash safety.
- [`docs/platform-support.md`](docs/platform-support.md) — OS support matrix and tray details.
- [`docs/luminar-catalog.md`](docs/luminar-catalog.md) — Luminar Neo catalog extraction.
- [branchDAM Agent Protocol](https://github.com/s3ntin3l8/branchdam/blob/main/docs/agent-protocol.md) — Wire contract and REST DTO specifications.

---

## Features & Subcommands

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
- `internal/luminar/`, `internal/nodeindex/` -- a `luminar-sync` subcommand: reads a Luminar Neo
  catalog read-only (`?mode=ro`, never `?immutable=1`; row extraction verified against a real
  catalog, `db_version 155`), infers edit->source pairs from filename convention -- the catalog
  itself stores no relational lineage to read directly -- and emits `EVENT_EDGE_ATTACHED` at
  `tier: 2, confidence: 0.89` for each pair it can resolve to known `nodeUuid`s. See
  [`docs/luminar-catalog.md`](docs/luminar-catalog.md) for the verification record, the
  zero-false-positive pairing measurement, and how to correct the query or the suffix heuristic
  against a different catalog.
- `internal/tray/`, `internal/autostart/`, `internal/selfupdate/`, `internal/appbundle/` -- the
  tray shell: a `fyne.io/systray` icon/menu (windows/darwin only) plus an embedded `net/http`
  status page showing watch directories, scratch-directory info, and queue status; login-item
  registration (off by default); `go-selfupdate` wiring (update *checks* on by default -- a
  read-only GitHub API call, `selfUpdate.enabled: false` opts out -- but *applying* one is
  always a separate explicit action) that notifies of an update and, on a menu click (or
  headless via the `update` subcommand), checksum-verifies, downloads, and applies it, then
  restarts the tray -- see
  [`docs/platform-support.md`](docs/platform-support.md) for the per-platform details and known
  gaps (including the queue-status stub). `internal/appbundle` assembles the macOS `.app`
  bundle both the release pipeline (`tools/mkbundle`) and self-update's `Info.plist` rewrite
  share.

## Install

Download the archive for your platform from the
[latest release](https://github.com/s3ntin3l8/branchdam-agent/releases/latest) and verify it
against the release's `SHA256SUMS.txt`:

| Platform | Asset | Contains |
|---|---|---|
| Linux (amd64) | `branchdam-agent-linux-amd64.tar.gz` | `branchdam-agent` -- headless subcommands only, no tray |
| Windows (amd64) | `branchdam-agent-windows-amd64.zip` | `branchdam-agent.exe` (console, for CLI use) + `branchdam-agent-tray.exe` (no console, for the tray/login-item launch path) |
| macOS (Apple Silicon) | `branchdam-agent-darwin-arm64.tar.gz` | `branchdam-agent.app` -- includes the tray; the CLI subcommands also work invoked directly at `branchdam-agent.app/Contents/MacOS/branchdam-agent` |

```sh
tar -xzf branchdam-agent-<platform>.tar.gz    # linux/darwin/macOS
sha256sum -c SHA256SUMS.txt                    # verify
```

Extracting with `tar` in a terminal (rather than a browser download + Archive Utility) avoids
macOS's quarantine attribute and the App Translocation it can trigger -- see
[`docs/platform-support.md`](docs/platform-support.md#macos-app-bundle). **On macOS, move
`branchdam-agent.app` to `/Applications` or `~/Applications`** before first launch; self-update
additionally requires a per-user install location (`~/Applications`, or
`%LOCALAPPDATA%\Programs\branchDAM\` on Windows) since it writes its own replacement binary --
see [`docs/platform-support.md`](docs/platform-support.md#self-update).

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

Or skip hand-copying the example: `branchdam-agent init` writes a starter config with empty
required fields (never `config.example.yaml`'s `${VAR}` placeholders, which would immediately trip
`preflight`'s validation) to the same default location described below, and prints what to edit
next. Refuses to overwrite an existing file unless `-force` is passed.

Every example below passes `-config` explicitly, which always takes precedence. Omitting it
entirely also works: every subcommand then falls back to `./config.yaml` if one exists in the
current directory, else the per-user config directory (`~/.config/branchdam-agent/config.yaml` on
Linux, `~/Library/Application Support/branchdam-agent/config.yaml` on macOS,
`%AppData%\branchdam-agent\config.yaml` on Windows) -- so a config placed there is found
automatically without a flag on every invocation.

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
go run ./cmd/branchdam-agent luminar-sync -catalog /path/to/catalog.luminarneo -node-index node-index.json -dry-run

# Confirm a real catalog's schema still matches the built-in query (see docs/luminar-catalog.md
# for the verification record before trusting it against a different Luminar Neo version):
go run ./cmd/branchdam-agent luminar-sync -catalog /path/to/catalog.luminarneo -dump-schema

go run ./cmd/branchdam-agent luminar-sync -config config.yaml -catalog /path/to/catalog.luminarneo -node-index node-index.json
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

**Offline queue.** When `offline.queueDbPath` is set, the tray opens `queue.db` itself and runs
drain and prune passes on their own background timers (`offline.drainIntervalSecs`, default 5s;
`prune.intervalMinutes`, default 30, only when `prune.enabled: true`) -- no separate
`queue-drain -watch`/`prune -watch` process is needed while the tray is running (see
[`docs/offline-queue.md`](docs/offline-queue.md)). "Drain queue now" and "Prune now" menu items run
the same passes on demand. The status page and menu show a real backlog count and permanently
failed count from `queue.db`, never a fabricated number when the queue isn't configured or can't be
read.

On Linux, `tray` builds and runs, but immediately returns an error (`tray: unsupported on this
platform`) -- the tray is scoped to Windows/macOS; a Linux workstation still has the fully-tested
headless `ingest` path.

**First run.** If no config exists yet, the tray no longer just exits: it writes a starter config
(same one `init` writes) and, where a dialog backend is available, walks a short setup wizard
(server URL, API key, the two ingest roots) before continuing. Every startup failure -- a broken
config, a bind conflict, an update that failed to restart -- is both logged to a durable per-OS log
file (`%LOCALAPPDATA%\branchDAM\logs\agent.log` on Windows, `~/Library/Logs/branchDAM/agent.log` on
macOS) and, best-effort, shown as a dialog naming that log path -- see issue #30 and
[`docs/platform-support.md`](docs/platform-support.md#startup-diagnostics-and-first-run-setup) for
what's verified and what isn't yet.

**Settings.** The tray menu's "Settings" submenu covers every commonly-changed field without
hand-editing `config.yaml`: checkboxes/submenus for start-at-login, update checking (and its
interval), and require-unbuffered-verify; dialogs for the server URL, API key, the two ingest
roots, and the naming template. Most changes apply immediately; a change to `tray.statusAddr` or
`ingest.cardRoots` shows "Restart now" instead, since neither can be hot-reloaded (see
[`docs/platform-support.md`](docs/platform-support.md)). Multi-value fields (`pathMappings`, multiple
`ingest.cardRoots`) stay hand-edit only -- "Open config.yaml" and "Reveal config folder" are right
there in the same submenu for exactly that. See
[`docs/platform-support.md`](docs/platform-support.md#settings-menu) for what's verified.

Self-update support is compiled into every build; no build tag is required. Checking is **on by
default** and periodic (`selfUpdate.enabled: false` opts out entirely) but passive -- it's a
read-only GitHub API call and never downloads or applies anything by itself. Installing is
always a separate, explicit action: a menu click ("Install and restart") that
checksum-verifies against the release's `SHA256SUMS.txt`, applies, and restarts the tray.
Headless hosts (Linux, or a Windows/macOS console-only install) get the same thing via
`branchdam-agent update -config
config.yaml [-check] [-yes]`.

**Rollback.** A successful apply keeps the version it replaced (a `.previous` backup next to each
binary, plus a version sidecar). The tray menu shows "Roll back to vX.Y.Z" whenever one is
available; the headless equivalent is `branchdam-agent update -rollback [-yes]`. Rollback makes no
network call at all, so it works even with `selfUpdate.enabled: false`. Once used (or once a new
`Apply` succeeds), the backup is consumed and the affordance disappears until the next update.

See [`docs/platform-support.md`](docs/platform-support.md) for the
full per-platform breakdown (why Windows ships two `.exe`s, the `fyne.io/systray` cross-compile
matrix, login-item registration, and known gaps like the unverified macOS Dock-icon behavior and
the queue-status stub).

## Development & Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for local development setup, testing (`make test`), linting (`make lint`), cross-compiling for Windows/macOS, and pull request guidelines.

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
