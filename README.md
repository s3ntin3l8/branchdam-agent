# branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam) (phase 10 of the
original spec) -- a Go binary that will eventually ingest SD cards, keep an offline queue, and
report to branchDAM over its existing `/api/v1/agent/*` REST contract. See branchDAM's
`.claude/plans/can-we-walk-through-sharded-lighthouse.md` for the full phased plan this repo
implements.

This is **M0**: the repo scaffold and the hand-written REST client every later milestone depends
on. There is no tray UI, card ingest, offline queue, or DaVinci/Luminar integration yet -- see
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
