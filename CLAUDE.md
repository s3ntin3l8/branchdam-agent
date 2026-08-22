# CLAUDE.md — branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam), phase 10 of the
original spec. Scaffolded from
[`s3ntin3l8/go-http-template`](https://github.com/s3ntin3l8/go-http-template); the template's
HTTP-server scaffolding (`cmd/server/`, `internal/httpapi/`, `Dockerfile`) was stripped in M0
because this is a desktop CLI/tray binary, not a service. If you are an AI agent or developer
working in this repo, read this first, then read branchDAM's
`.claude/plans/can-we-walk-through-sharded-lighthouse.md` -- the approved phase-10 plan this
whole repo implements, with far more detail than any single issue.

## What this repo is, and isn't (yet)

branchDAM's server side has shipped the full `/api/v1/agent/*` contract (event queue drainer,
handshake, path rebase) since phase 8, but until this repo exists, nothing speaks it. Every new
master currently reaches the graph only via an operator-triggered scan; SD-card ingest has no
bit-for-bit verification or safe-eject signalling; stills lineage has no confidence-1.00 path.
This repo closes that gap, in milestones:

- **M0 (this PR)** -- repo scaffold, the `internal/branchdam/` REST client, three ported pieces
  of server logic (`FastHash`, `PerceptualHash`'s call sequence, `naming.Stem`), and a
  `preflight` subcommand. No SQLite, no tray, no ingest.
- **M1** -- tray shell + SD-card ingest core (one read, two writes, BLAKE3-256 verify against an
  unbuffered re-read, DJI `.srt` GPS parsing).
- **M2** -- offline queue (`modernc.org/sqlite`) + the rebase handoff.
- **M3** -- DaVinci Resolve post-render hook (Python, `hooks/resolve/`).
- **M4** -- Luminar `catalog.db` reader.

See the plan doc for the full reasoning behind each (notably: why Go and not Rust/Tauri --
`internal/hashing.PerceptualHash` has to be bit-identical to branchDAM's own, which only holds if
both sides call the same `goimagehash` library).

## First steps after creating a repo from this template (already done for M0)

1. Rename the placeholders: `module` in `go.mod`, the `# Project Name` title in `README.md`, and
   the `module` path across `.go` files. Done: `github.com/s3ntin3l8/branchdam-agent`.
2. `make install-hooks` -- installs pre-commit and pre-push hooks.
3. `make build` -- verify everything compiles.
4. Decide your CI coverage floor: `ci-cd.yml` ships `coverage-fail-under: '0'` -- ratchet it up
   as real ingest code lands in M1+.

## Commands (Makefile)

| Command | Does |
|---------|------|
| `make install-hooks` | Install pre-commit + pre-push hooks. |
| `make test` | Run Go tests with race detection and coverage. |
| `make lint` | Run pre-commit on all files (golangci-lint + go vet + detect-secrets). |
| `make fmt` | Format Go code with gofmt (and goimports if available). |
| `make vet` | Run go vet. |
| `make tidy` | Run go mod tidy. |
| `make vulncheck` | Run govulncheck for known vulnerabilities. |
| `make build` | Build all packages. |
| `make clean` | Remove build artifacts and test caches. |

golangci-lint also runs as its own pinned CI job (`v2.12.2`, matching `.golangci.yml`'s
`version: "2"` schema) separately from whatever `ci-go.yml`'s own lint step covers -- mirroring
branchDAM's own `ci.yml` pattern, since `.golangci.yml` here requires a matching v2.x binary, not
whatever "latest" resolves to.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/branchdam-agent/main.go` | Entrypoint + subcommand dispatch (`preflight`, `version`) |
| `cmd/branchdam-agent/preflight.go` | `preflight`'s checks (server reachability/version, `exiftool` on `PATH`, path mappings), factored out of `main.go` so it's testable without capturing stdout |
| `internal/branchdam/` | The REST client for branchDAM's `/api/v1/agent/*` contract -- one file per endpoint (`hello.go`, `handshake.go`, `events.go`, `rebase.go`), `types.go` for the hand-synced DTOs, `errors.go` for client-side validation gates and fatal/transient error classification, `conformance_test.go` + `testdata/*.golden.json` for the byte-for-byte fixture tests |
| `internal/hashing/` | Byte-for-byte port of branchDAM's `FastHash` (sampled xxHash64) and `PerceptualHash` (thin `goimagehash.PerceptionHash` wrapper) |
| `internal/naming/` | Byte-for-byte port of branchDAM's `naming.Stem`/`naming.Analyze` filename normalization |
| `internal/phash/` | Port of branchDAM's `probe.ExtractPHash` *call sequence* (direct decode, then exiftool `-PreviewImage`/`-JpgFromRaw`/`-ThumbnailImage` fallback, first decodable wins) -- not a reimplementation of exiftool itself |
| `internal/config/` | YAML config loader: branchDAM server URL + API key, this workstation's self-asserted `agentId`, and the workstation-path -> container-path map `preflight` prints |
| `config.example.yaml` | Reference config with `${VAR}` placeholders |
| `.github/workflows/` | Thin callers of the reusable workflows in `s3ntin3l8/.github`, minus the Docker jobs the template ships (no image is published from this repo) |
| `.editorconfig` | Shared editor settings (LF, UTF-8, final newline; tabs for Go) |
| `.claude/` | `settings.json` + `hooks/session-start.sh`: a SessionStart hook that installs Go deps and tooling so [Claude Code on the web](https://code.claude.com/docs/en/claude-code-on-the-web) sessions can build, test, and lint. Runs only in the remote env |

## Key invariants

- **`internal/branchdam`'s DTOs are hand-synced, not generated.** Nothing under branchDAM's
  `internal/` is importable cross-module (different Go module entirely), so every struct in
  `internal/branchdam/types.go` is a manually-maintained mirror of branchDAM's
  `internal/agent/types.go` and `internal/httpapi/routes.go` agent DTOs -- the same accepted
  boundary as branchDAM's own `web/src/api/types.ts` vs. its Go backend. `ContractVersion`
  (`types.go`) pins the branchDAM commit this mirror was built against; bump it deliberately,
  alongside the structs, whenever the upstream DTOs change -- never let it drift silently.
- **`hello`'s response field is `"version"`; `handshake`'s is `"serverVersion"`.** Same concept,
  different JSON field name on two different branchDAM endpoints -- `HelloResponse` and
  `HandshakeResponse` are deliberately separate Go types, never a shared struct, so a request
  against one endpoint can't silently decode an empty string by reading the other's field name.
  `TestHelloVsHandshakeFieldNamesDiffer` (`internal/branchdam/conformance_test.go`) is the
  regression test.
- **`/api/v1/agent/events`' `payload` field is a JSON *string* (double-encoded); `evidenceJson`
  inside an `EVENT_EDGE_ATTACHED` payload is a JSON *object*.** Opposite encodings, one level
  apart in the same request -- the single most confusable pair in the whole contract per the
  plan doc. `TestConformanceEventEnvelopeDoubleEncoding` pins both in one fixture so a future
  refactor can't flip either without a golden-file diff.
  `internal/branchdam.Client.postEvent` is the only place that performs the double-encoding;
  every typed `PostXxx` method goes through it.
- **`EVENT_EDGE_ATTACHED`'s hard gates are validated client-side, before any POST.** The server
  never validates this payload at enqueue time (202 regardless of content) and has no failure
  feedback channel, so `internal/branchdam.ValidateEdgeAttached` (called from
  `Client.PostEdgeAttached`) is the only place a bad `confidence` (must be in `(0,1]` **and**
  `>= 0.50`), `tier` (must be 1/2/3), `relationshipType` (one of five values), or a set
  `reviewState` (never allowed from an agent) is caught before it burns the server's retry
  budget and lands `FAILED` invisibly.
- **All four agent routes are POST-only.** `internal/branchdam.Client` never issues a GET against
  any of them; a bare `curl` GET against `hello` returns 405 on the real server (a known doc bug
  in branchDAM's own `docs/deploy.md`/`docs/forward-auth.md`, not something to replicate here).
- **401/503 auth-failure bodies are plain text, not JSON.** `internal/branchdam.HTTPError.Body`
  is always the raw response text; nothing in this client assumes a parsed JSON error shape for
  those two status codes.
- **Event submission is not idempotent at the transport level.** branchDAM mints its own
  `event_uuid` server-side (`AgentEventInput` has no client-suppliable event ID field) -- a
  retry after a timeout enqueues a *second* row. Later milestones (M2's offline queue) must rely
  on entity-level idempotency (a re-sent `EVENT_NODE_CREATED` for an existing `nodeUuid` is a
  silent no-op) and mint a stable `nodeUuid` before the first attempt, not on any request-level
  dedup this client could provide.
- **The three ported algorithms (`FastHash`, `PerceptualHash`, `naming.Stem`) are copies of
  branchDAM's implementation, not reimplementations from the spec.** Nothing under branchDAM's
  `internal/` is importable cross-module. `internal/hashing/hashing_test.go` and
  `internal/naming/naming_test.go`'s golden vectors are transcribed from branchDAM's own
  committed test tables (themselves produced by running branchDAM's real implementation);
  `internal/hashing/hashing_test.go`'s `PerceptualHash` vectors and the `testdata/gradient.*`
  fixtures were produced by a throwaway harness run against a real branchDAM checkout (documented
  in that test file's doc comment, not committed to branchDAM). Regenerating any of these
  requires running branchDAM's actual code again, not deriving values from the spec prose.
- **`internal/phash` ports the *call sequence*, not exiftool itself.** No RAW test fixtures are
  committed (none were available, and reproducing exiftool's own RAW parsing is out of scope) --
  `internal/phash/phash_test.go` instead uses a fake `exiftool` shell script to assert the fixed
  three-tag fallback order (`-PreviewImage`, `-JpgFromRaw`, `-ThumbnailImage`) and "first
  decodable output wins," plus a real fixture image to prove the direct-decode fast path never
  shells out at all.
- **`config.example.yaml`'s values must be literal, not `${VAR:-default}`.** Same `expandEnv`
  limitation as the go-http-template this repo was scaffolded from (`internal/config/config.go`)
  -- captures everything between `${` and `}` as one literal environment-variable name, no
  `:-default` support.
- **Agent payload paths are server-container, absolute, symlink-free -- there is no server-side
  rewrite pass on them.** `internal/config.PathMapping` is what a future ingest milestone
  translates a workstation path through *before* it ever appears in a request body;
  `preflight` prints the configured mappings so an operator can eyeball them first. This is the
  opposite convention from `.dam.json`'s `raw_path` (workstation-native, translated server-side)
  that M3's Resolve hook will use -- deliberately called out here since the plan doc flags it as
  the single most confusable thing across the whole phase-10 design.

## CI/CD — uses centralized reusable workflows

Workflows here are **callers** of `s3ntin3l8/.github/.github/workflows/*.yml@main`: `ci-cd.yml`
(`ci-go` only -- no `build-docker`, unlike the template), `codeql.yml`, `dependency-review.yml`,
`release-please.yml` (release-please only, no Docker publish step -- a later milestone adds a
real cross-platform release matrix for Windows/macOS packaging, not this repo's CI today).

**The #1 thing to get right:** a caller job that invokes a reusable workflow needing write scopes
**must declare a `permissions:` block** -- the default `GITHUB_TOKEN` is read-only and the run
otherwise fails at startup with zero jobs. `release-please` needs `contents: write` +
`pull-requests: write`; `codeql` needs `security-events: write`.

`ci-go` reads the Go version from `go.mod`, runs gofmt, go vet, go build, go test -race with
coverage, and govulncheck.

> **Codecov TODO:** coverage upload requires a `CODECOV_TOKEN` repo secret and the repo onboarded
> on [codecov.io](https://about.codecov.io/) before results/badges show.

## Conventions

- **Go 1.26+, stdlib-first** where possible; `internal/branchdam` uses only `net/http` +
  `encoding/json`, no HTTP client framework.
- **Conventional Commits** -- Release Please cuts versions/changelogs from them.
- **Linting enforced** by golangci-lint and go vet (config in `.pre-commit-config.yaml`); run
  `make lint` before pushing (the pre-push hook runs govulncheck).
- **Secrets:** never commit real credentials; `detect-secrets` runs in pre-commit and CI against
  `.secrets.baseline` (regenerate with `detect-secrets scan > .secrets.baseline` after vetting
  new detections -- expect this after adding a fixture containing a 32+ char test API key).

## Documentation map

- `README.md` -- setup and usage instructions.
- `CLAUDE.md` (this file) -- the live contract for how the repo is wired: layout, invariants,
  CI/CD conventions, milestone map. Keep it in sync with the code, not with what the code used to
  do.
- branchDAM's `.claude/plans/can-we-walk-through-sharded-lighthouse.md` -- the approved phase-10
  design this whole repo implements; the source of truth for anything not covered above.
