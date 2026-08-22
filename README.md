# branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) (phase 10 of the
original spec) -- a Go binary that will eventually ingest SD cards, keep an offline queue, and
report to branchDAM over its existing `/api/v1/agent/*` REST contract. See branchDAM's
`.claude/plans/can-we-walk-through-sharded-lighthouse.md` for the full phased plan this repo
implements.

Landed so far: **M0** (repo scaffold + REST client), the SD-card ingest core half of **M1**
(dual-copy verified write, metadata extraction, headless `ingest` -- no tray UI yet), **M2**
(offline queue + rebase handoff, this PR), **M3** (DaVinci Resolve post-render hook,
`hooks/resolve/`), and **M4** (Luminar `catalog.db` reader). There is still no tray UI -- see
[CLAUDE.md](CLAUDE.md) for the milestone breakdown.

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
- `internal/ingest/` -- the SD-card ingest core: one-read/two-write dual-copy writer, a
  cache-defeating verified re-read (`fsync`+close+reopen floor, unbuffered/`O_DIRECT` where the
  platform supports it), DJI `.srt` telemetry parsing for the video's own GPS fields, and
  submission via `internal/branchdam`. Headless via `ingest -card <path>`.
- `internal/queue/`, `internal/ingest`'s `IngestCardOffline`/`Drain` -- M2's offline queue
  (`ingest -offline`, `queue-drain`): every intended event persisted to `queue.db`
  (`modernc.org/sqlite`) before any network call, so a workstation with no route to the NAS can
  still ingest a card, then finish the archive copy and `POST /api/v1/agent/rebase` once
  reconnected -- see [`docs/offline-queue.md`](docs/offline-queue.md) for the full state machine,
  the copy-before-rebase ordering guarantee, and the server-side prerequisite this depends on
  (**not yet deployed** as of this PR -- see that doc's first section).
- `internal/luminar/`, `internal/nodeindex/` -- a `luminar-sync` subcommand: reads a Luminar
  `catalog.db` read-only (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` at
  `tier: 2, confidence: 0.89` for each edit->source pair it finds and can resolve to known
  `nodeUuid`s. Luminar's schema is undocumented -- see
  [`docs/luminar-catalog.md`](docs/luminar-catalog.md) for the research, confidence level, and how
  to correct the query against a real catalog.

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
| `make build` | Build all packages. |
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
