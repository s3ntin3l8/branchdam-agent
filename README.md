<p align="center">
  <img src="docs/img/logo.svg" alt="branchdam-agent logo" width="64" height="64">
</p>

# branchdam-agent

The workstation companion agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) — a cross-platform desktop application and CLI that handles SD card media ingestion, dual-copy verified storage writes, offline queueing, and catalog synchronization.

---

## Core Capabilities

- **Bit-for-Bit Verified Ingest**: Dual-write engine reads camera SD cards once and streams simultaneously to your NAS archive (Tier 3) and fast local NVMe edit drive (Tier 1), computing BLAKE3-256 and xxHash64 checksums in flight. Flushes and performs cache-busting unbuffered re-reads (`O_DIRECT` / `F_NOCACHE`) to guarantee data integrity.
- **Direct HTTP Streaming Ingest**: Stream media straight to branchDAM's Master Archive over `POST /api/v1/agent/upload` without requiring local SMB or NFS share mounts. Naming templates are synchronized dynamically via server handshake (`POST /api/v1/agent/handshake`) with 100% path parity across local edit scratch and remote archive.
- **Offline Field Ingest**: Ingest media on the go without network access to your NAS or server. Events and local files are safely recorded in a local SQLite queue (`queue.db`), then automatically copied to the archive and rebased upon reconnecting to your LAN.
- **System Tray & GUI (Windows & macOS)**: Lightweight menu bar / system tray companion providing auto card-detection, real-time queue status, and a native Settings UI for paths and server credentials.
- **Catalog & NLE Synchronization**:
  - **Skylum Luminar Neo**: `luminar-sync` reads a local `.luminarneo` catalog and infers derived-file->source links from filename convention.
  - **DaVinci Resolve**: Post-render Python hook (`hooks/resolve/`) writes `.dam.json` sidecars for confidence-1.00 timeline-to-export lineage.
  - **Local Mirror Pruning**: `prune` safely reclaims local edit scratch space once branchDAM confirms the master archive copy is hash-verified.
  - **30-Day Trash Buffer Lifecycle**: Deleted assets (`EVENT_NODE_DELETED`) are safely isolated in branchDAM's `.trash/` buffer on the server, purging Immich galleries immediately while preserving master files for 30 days.

---

## Architecture & Wire Protocol

The agent communicates with the branchDAM server via its `/api/v1/agent/*` REST API contract with static `X-API-Key` authentication. See:
- [`docs/offline-queue.md`](docs/offline-queue.md) — Offline queue state machine and crash safety.
- [`docs/platform-support.md`](docs/platform-support.md) — OS support matrix and tray details.
- [`docs/luminar-catalog.md`](docs/luminar-catalog.md) — Luminar Neo catalog extraction.
- [branchDAM Agent Protocol](https://github.com/s3ntin3l8/branchdam/blob/main/docs/agent-api.md) — Wire contract and REST DTO specifications.

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
  status server (`/`, `/status`, `/status.json` -- plain JSON, no browsable page; see the Wails
  window below for the settings/status UI) reporting watch directories, scratch-directory info,
  and queue status; login-item
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
against the release's `SHA256SUMS.txt`. Every asset's filename embeds the release version (e.g.
`branchdam-agent-v1.9.0-windows-amd64.zip`), so downloads from different releases never collide in
`~/Downloads`; `SHA256SUMS.txt` keeps this one fixed, unversioned name across every release, since
self-update matches it by exact filename, not by pattern:

| Platform | Asset | Contains |
|---|---|---|
| Linux (amd64) | `branchdam-agent-<version>-linux-amd64.tar.gz` | `branchdam-agent` -- headless subcommands only, no tray |
| Windows (amd64) | `branchdam-agent-<version>-setup.exe` | **Recommended.** NSIS installer: extracts binaries, creates shortcuts, writes starter config, adds to Start Menu and Add/Remove Programs |
| Windows (amd64) | `branchdam-agent-<version>-windows-amd64.zip` | Self-update's own download payload (and a manual-extract fallback): `branchdam-agent.exe` (console, for CLI use) + `branchdam-agent-tray.exe` (no console, for the tray/login-item launch path). Not the recommended way to install -- use the installer above. |
| macOS (Apple Silicon) | `branchdam-agent-<version>-darwin-arm64.dmg` | **Recommended.** `branchdam-agent.app` (includes the tray) in a disk image with an `Applications` shortcut for drag-to-install -- see Manual Installation below |
| macOS (Apple Silicon) | `branchdam-agent-<version>-darwin-arm64.tar.gz` | Self-update's own download payload. Not the recommended way to install -- use the `.dmg` above. |

### Windows Installer (Recommended)

`branchdam-agent-<version>-setup.exe` is the supported way to install on Windows -- the
`branchdam-agent-<version>-windows-amd64.zip` on the same release is what self-update itself
downloads, not a recommended manual-install path.

1. Download `branchdam-agent-<version>-setup.exe` from the latest release
2. Run the installer - it will:
   - Extract binaries to `%LOCALAPPDATA%\Programs\branchDAM\`
   - Create Start Menu shortcuts
   - Write a starter config with your computer name as the agent ID
   - Add the agent to Add/Remove Programs, with a real version number
3. On finish, check "Launch branchDAM Agent" to start the tray
4. The tray starts in **"not configured" mode** (gray icon) - click "Open branchDAM" in the tray
   menu to open the native window and configure:
   - Set your server URL, API key, and agent ID
   - Configure path mappings (archive root, edit root, container paths)
   - Set card detection roots and other preferences

The installer does not collect any configuration at install time (except the default agent ID) - all settings are configured through the native branchDAM window after installation.

> **Note:** Uninstalling via Add/Remove Programs preserves `%APPDATA%\branchdam-agent\config.yaml`. The installer also preserves it on upgrade (`IfFileExists` guard). Uninstall-then-reinstall keeps your settings. If branchDAM Agent is currently running, installing or uninstalling over it prompts you to quit it (right-click the tray icon → Quit) and retry, rather than silently failing to update the files or leaving the install directory behind.

### macOS Installation (Recommended)

1. Download `branchdam-agent-<version>-darwin-arm64.dmg` from the latest release and open it
2. Drag `branchdam-agent.app` onto the `Applications` shortcut in the same window
3. Eject the disk image, then launch branchDAM Agent from `/Applications`
4. The bundle is ad-hoc signed (not notarized), so first launch shows macOS's ordinary
   unidentified-developer warning, not a "damaged" error -- right-click the app and choose
   **Open** to get past it. See
   [`docs/platform-support.md`](docs/platform-support.md#ad-hoc-signing-not-notarization) for
   what ad-hoc signing does and doesn't cover.

Self-update needs a per-user install location to have write access
(`~/Applications`, not `/Applications`) -- see
[`docs/platform-support.md`](docs/platform-support.md#self-update).

### Manual Installation

```sh
tar -xzf branchdam-agent-<version>-<platform>.tar.gz    # linux/darwin/macOS
sha256sum -c SHA256SUMS.txt                              # verify
```

On Linux and for scripted/headless installs, extract the `.tar.gz` directly. On macOS, prefer
the `.dmg` above -- the `.tar.gz` is what self-update itself downloads, not the recommended
manual-install path. Extracting it with `tar` in a terminal (rather than a browser download +
Archive Utility) avoids macOS's quarantine attribute and the App Translocation it can trigger --
see [`docs/platform-support.md`](docs/platform-support.md#macos-app-bundle). **On macOS, move
`branchdam-agent.app` to `/Applications` or `~/Applications`** before first launch; self-update
additionally requires a per-user install location (`~/Applications`, or
`%LOCALAPPDATA%\Programs\branchDAM\` on Windows) since it writes its own replacement binary --
see [`docs/platform-support.md`](docs/platform-support.md#self-update).

Binaries are unsigned on Windows and Linux, and only ad-hoc signed (not notarized) on macOS --
see "Releases" below. See
[`docs/platform-support.md`](docs/platform-support.md) for the full support matrix, including
why Windows ships two `.exe`s and what's not yet implemented per platform.

On Apple Silicon, GUI launches do not reliably inherit Homebrew's `/opt/homebrew/bin` PATH. Set
`ingest.exiftoolPath: "/opt/homebrew/bin/exiftool"` when ExifTool is installed there; ingestion
continues without it, but EXIF metadata and RAW-preview perceptual hashing are reduced.

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

### 4. Ingest an SD card, including direct HTTP streaming and offline

```sh
# Online (server reachable with local SMB/NFS mount): dual-copy write, immediate EVENT_NODE_CREATED.
go run ./cmd/branchdam-agent ingest -config config.yaml -card /media/$USER/UNTITLED

# Direct HTTP streaming (no SMB/NFS share mount required): streams to server Master Archive.
go run ./cmd/branchdam-agent ingest -config config.yaml -card /media/$USER/UNTITLED -upload

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

**Config fallback.** `-catalog` and `-node-index` fall back to `integrations.luminar.catalogPath`
and `integrations.nodeIndexPath` in config when the flag isn't passed at all, so a config the tray
also uses (once wired up) works for the CLI too -- an explicit flag always wins. `-dry-run` gets
**no** config fallback, deliberately: `integrations.luminar.dryRun` defaults to `true` even when
`config.yaml` has no `integrations:` block at all, so falling back would silently turn every
already-scripted `luminar-sync -catalog X -node-index Y` invocation (cron, CI, ...) into a no-op
that still exits 0. `-dry-run` has always meant "preview, don't contact the server" here, and
omitting it has always meant "do it" -- that stays true regardless of config. The config key
governs only the tray's own timer-driven sync, once that lands. `-query-file`/`-derivative-suffixes`
are also CLI-only, on purpose -- see [`docs/luminar-catalog.md`](docs/luminar-catalog.md).

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

Starts the tray icon (windows/darwin) plus an embedded JSON status endpoint (default
`http://127.0.0.1:38080/`, loopback-only -- see `tray.statusAddr` in `config.example.yaml`)
reporting configured watch directories, the local scratch directory, queue status, each catalog
integration's config/last-sync state, and the DaVinci Resolve render hook's installed state -- see
"Open branchDAM" below for the actual settings/status UI. The
tray menu's "Ingest now" and its automatic card-insertion trigger both call the same
`internal/ingest.Engine.IngestCard` the headless `ingest` subcommand uses.

**Offline queue.** When `offline.queueDbPath` is set, the tray opens `queue.db` itself and runs
drain and prune passes on their own background timers (`offline.drainIntervalSecs`, default 5s;
`prune.intervalMinutes`, default 30, only when `prune.enabled: true`) -- no separate
`queue-drain -watch`/`prune -watch` process is needed while the tray is running (see
[`docs/offline-queue.md`](docs/offline-queue.md)). "Drain queue now" and "Prune now" menu items run
the same passes on demand. The Wails window and tray tooltip show a real backlog count and
permanently failed count from `queue.db`, never a fabricated number when the queue isn't
configured or can't be read.

**Integrations.** Luminar Neo and the DaVinci Resolve database sync each get a **top-level** tray
menu item (e.g. "Luminar Neo"), not nested under Settings: an Enabled checkbox, a Dry run checkbox
(defaults **on** -- resolves and logs what a pass would emit without contacting the server, until an
operator turns it off explicitly), a catalog file picker, a "Sync every" submenu (15 min / 60 min
default / manual-only), and "Sync now". A shared "Node index…" item (the JSON file mapping
workstation paths to `nodeUuid`s every catalog integration resolves against) sits alongside them.
The tray also runs each enabled integration's sync on its own background timer
(`integrations.luminar.syncIntervalMinutes`), independent of the ingest/drain/prune timers so a sync
pass never blocks a card ingest and vice versa -- see `config.example.yaml`'s `integrations:` block
for every field. Every menu-driven change applies immediately, no restart required.

Resolve database sync reads a configured or auto-discovered SQLite database, or a configured
PostgreSQL Project Server, using read-only connections. Each successful pass sends the full
timeline snapshot to the server's synchronous `/api/v1/agent/resolve-snapshot` endpoint: indexed
media edges are created/refreshed and removed memberships make unreviewed edges inactive (retained
for audit). Human-reviewed edges remain untouched and are flagged for manual resolution. A server
without this endpoint returns an actionable sync error; there is no legacy event fallback. The
database URL prompt is hidden and non-prefilled so credentials do not appear in process arguments
or in the JSON status output. See [Resolve validation](docs/resolve-projectdb.md) before live deployment.

**DaVinci Resolve render hook.** An installer, not a sync integration, since the hook itself runs
inside Resolve's own Python interpreter and takes no config beyond an optional
`integrations.resolve.scriptsDir` override (see [`hooks/resolve/README.md`](hooks/resolve/README.md)).
A top-level "DaVinci Resolve" tray menu item -- a sibling of the catalog-integration items, not
nested under them -- covers this without ever touching a terminal:

```
DaVinci Resolve
├─ "installed and up to date" / "not installed" / ...   [disabled, cached at startup]
├─ Install / update render hook
└─ Reveal Scripts folder
```

Installing always targets the most-writable candidate directory (the per-user `Scripts/Utility`
folder on macOS, so an unprivileged operator never hits `EACCES` against the admin-owned
system-wide one) unless the hook is already installed somewhere else, in which case it reinstalls
in place. The status line is checked once at startup and refreshed after every "Install / update"
click -- never on the regular 5s menu-refresh tick, since a filesystem stat plus a checksum read on
a possibly-networked `scriptsDir` could otherwise hang the whole menu. The same install logic is
also available headlessly, for a workstation that never runs the tray:

```sh
branchdam-agent resolve-hook -install [-dir <path>] [-config <path>]
```

On Linux, `tray` builds and runs, but immediately returns an error (`tray: unsupported on this
platform`) -- the tray is scoped to Windows/macOS; a Linux workstation still has the fully-tested
headless `ingest` path.

**First run.** If no config exists yet, the tray no longer just exits: it writes a starter config
(same one `init` writes) and continues in gray **not configured** mode. Use Settings to provide the
server URL, API key, agent ID, and ingest destinations. Every startup failure -- a broken
config, a bind conflict, an update that failed to restart -- is both logged to a durable per-OS log
file (`%LOCALAPPDATA%\branchDAM\logs\agent.log` on Windows, `~/Library/Logs/branchDAM/agent.log` on
macOS) and, best-effort, shown as a dialog naming that log path -- see issue #30 and
[`docs/platform-support.md`](docs/platform-support.md#startup-diagnostics-and-first-run-setup) for
what's verified and what isn't yet.

**Settings.** All commonly-changed fields are configured through the native branchDAM window
("Open branchDAM" in the tray menu), not through hand-editing `config.yaml`: checkboxes for
start-at-login, update checking (and its interval), and require-unbuffered-verify; text fields for
the server URL, API key, the two ingest roots, and the naming template. Most changes apply
immediately; a change to `tray.statusAddr` or `ingest.cardRoots` needs a restart, since neither can
be hot-reloaded (see [`docs/platform-support.md`](docs/platform-support.md)). Multi-value fields
(`pathMappings`, multiple `ingest.cardRoots`) stay hand-edit only -- the tray's own "Advanced"
submenu's "Open config.yaml" and "Reveal config folder" are there for exactly that. See
[`docs/platform-support.md`](docs/platform-support.md#advanced-menu-formerly-settings) for what's
verified.

Self-update support is compiled into every build; no build tag is required. Checking is **on by
default** and periodic (`selfUpdate.enabled: false` opts out entirely) but passive -- it's a
read-only GitHub API call and never downloads or applies anything by itself. Installing is
always a separate, explicit action: confirm "Install and restart" in either the tray menu or the
native branchDAM window. The action checksum-verifies against the release's `SHA256SUMS.txt`,
applies, and restarts the tray.
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

**No paid code-signing certificate is purchased for either platform.** Windows binaries are
unsigned; expect a SmartScreen warning on first run. The macOS bundle is ad-hoc signed in CI (see
[`docs/platform-support.md`](docs/platform-support.md#ad-hoc-signing-not-notarization)) but not
notarized, so it still shows Gatekeeper's ordinary unidentified-developer warning -- right-click
the app and choose **Open** to get past it.

## License

AGPL-3.0
