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

- **M0** -- repo scaffold, the `internal/branchdam/` REST client, three ported pieces
  of server logic (`FastHash`, `PerceptualHash`'s call sequence, `naming.Stem`), and a
  `preflight` subcommand. No SQLite, no tray, no ingest.
- **M1 (this PR, ingest-core half only -- see below)** -- SD-card ingest core: card detection
  (poll-based), one read/two writes, BLAKE3-256 verify against a cache-defeating re-read, DJI
  `.srt` GPS parsing, metadata extraction at promoted-column parity with a server-side scan, and
  the headless `ingest` subcommand. The tray shell itself (`fyne.io/systray` + embedded web UI)
  is deliberately a separate, later PR -- issue #2 scoped it out explicitly, and this package has
  no UI imports for exactly that reason: the tray, when it lands, is a second, thin driver over
  `internal/ingest.Engine`, not a rewrite of it.
- **M2** -- offline queue (`modernc.org/sqlite`) + the rebase handoff.
- **M3** -- DaVinci Resolve post-render hook (Python, `hooks/resolve/`).
- **M4 (this PR)** -- Luminar `catalog.db` reader (`internal/luminar/`, `internal/nodeindex/`,
  `luminar-sync` subcommand). Schema mapping is unverified -- see `docs/luminar-catalog.md`.

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
| `make check` | One-shot pre-PR gate: build + vet + test (does not run `lint` -- that needs `pre-commit` installed, see `make install-hooks`). |
| `make clean` | Remove build artifacts and test caches. |

golangci-lint also runs as its own pinned CI job (`v2.12.2`, matching `.golangci.yml`'s
`version: "2"` schema) separately from whatever `ci-go.yml`'s own lint step covers -- mirroring
branchDAM's own `ci.yml` pattern, since `.golangci.yml` here requires a matching v2.x binary, not
whatever "latest" resolves to.

## Layout

| Path | Responsibility |
|---|---|
| `cmd/branchdam-agent/main.go` | Entrypoint + subcommand dispatch (`preflight`, `luminar-sync`, `ingest`, `version`) |
| `cmd/branchdam-agent/preflight.go` | `preflight`'s checks (server reachability/version, `exiftool` on `PATH`, path mappings), factored out of `main.go` so it's testable without capturing stdout |
| `cmd/branchdam-agent/luminarsync.go` | `luminar-sync`'s flag parsing and orchestration (open catalog, load node index, run `luminar.Syncer`, print a summary); `--dump-schema` mode for recovering a real catalog's schema |
| `cmd/branchdam-agent/ingest.go` | `ingest --card <path> --config <path>`: the headless driver over `internal/ingest.Engine`, and the report printer that decides the process exit code / "safe to eject" line |
| `internal/branchdam/` | The REST client for branchDAM's `/api/v1/agent/*` contract -- one file per endpoint (`hello.go`, `handshake.go`, `events.go`, `rebase.go`), `types.go` for the hand-synced DTOs, `errors.go` for client-side validation gates and fatal/transient error classification, `conformance_test.go` + `testdata/*.golden.json` for the byte-for-byte fixture tests |
| `internal/hashing/` | Byte-for-byte port of branchDAM's `FastHash` (sampled xxHash64) and `PerceptualHash` (thin `goimagehash.PerceptionHash` wrapper), plus M1's own addition `StreamingFastHasher` -- a single-pass, `io.Writer`-shaped variant proven byte-identical to `FastHash` by an equivalence test, so the M1 dual-copy writer never re-reads the card a second time just to compute `fast_hash` |
| `internal/naming/` | Byte-for-byte port of branchDAM's `naming.Stem`/`naming.Analyze` filename normalization |
| `internal/phash/` | Port of branchDAM's `probe.ExtractPHash` *call sequence* (direct decode, then exiftool `-PreviewImage`/`-JpgFromRaw`/`-ThumbnailImage` fallback, first decodable wins) -- not a reimplementation of exiftool itself |
| `internal/luminar/` | Reads a Luminar `catalog.db` (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` for edit->source pairs; the actual (unverified) schema query is isolated in `query.go` and overridable via `--query-file` -- see `docs/luminar-catalog.md` |
| `internal/nodeindex/` | Maps a file path to the `nodeUuid` it was ingested as (`Resolver` interface, `FileIndex` JSON-file implementation) -- works around there being no agent-reachable lookup-by-path endpoint on branchDAM |
| `internal/djisrt/` | Byte-for-byte port of branchDAM's `internal/djisrt` -- DJI `.srt` first-GPS-fix telemetry parser, including `isValidFix`'s pre-lock `(0,0)` rejection |
| `internal/ingest/` | M1's SD-card ingest core, UI-free: `carddetect.go` (poll-based removable-volume detection), `writer.go` (one-read/two-write `DualWrite`), `verify.go`+`verify_linux.go`+`verify_other.go` (cache-defeating re-read, O_DIRECT on Linux with a documented buffered-floor fallback), `metadata.go` (exiftool extraction ported from branchDAM's `probe.go`, sidecar merge, `capturedAt` fallback chain), `srt.go` (DJI GPS-on-video wiring over `internal/djisrt`), `naming.go` (destination path template), `pathmap.go` (workstation->container path translation), `extensions.go` (video/image extension sets, copied from branchDAM's `pipeline.videoExts`/`imageExts` for parity), `ingest.go` (`Engine`, the orchestrator both the CLI and a later tray drive) |
| `internal/config/` | YAML config loader: branchDAM server URL + API key, this workstation's self-asserted `agentId`, the workstation-path -> container-path map `preflight` prints, and (M1) `ingest:` -- archive/local-edit roots, the naming template, and card-detection polling |
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
  rewrite pass on them.** `internal/config.PathMapping` + `internal/ingest.ToContainerPath`
  (longest-prefix match) is what M1's ingest core translates a written archive path through
  *before* it ever appears in a request body; `preflight` prints the configured mappings so an
  operator can eyeball them first. This is the opposite convention from `.dam.json`'s `raw_path`
  (workstation-native, translated server-side) that M3's Resolve hook will use -- deliberately
  called out here since the plan doc flags it as the single most confusable thing across the
  whole phase-10 design.
- **`internal/ingest.DualWrite` never re-reads the card to compute `fast_hash`.** It streams the
  source into both destinations plus `hashing.StreamingFastHasher` and `blake3.New()`
  simultaneously via `io.MultiWriter`, in one pass -- `StreamingFastHasher` captures exactly the
  bytes `hashing.FastHash`'s `sampleRegions` would read (including the overlapping-window case
  for a sub-6MiB file), proven byte-identical by
  `TestStreamingFastHasherMatchesFastHash` (`internal/hashing/hashing_test.go`). `DualWrite` also
  refuses to overwrite an existing destination (`O_EXCL`) -- a caller retrying a failed ingest
  must remove or rename the partial destination itself first.
- **Verify decides buffered-vs-O_DIRECT once, at open time, and never falls back mid-stream.**
  `internal/ingest.Verify` re-reads a file DualWrite already `fsync`'d and closed; on Linux
  (`verify_linux.go`) it tries `O_DIRECT` first and falls back to a plain reopen only if the
  *open itself* fails (the expected case on tmpfs and some network/overlay filesystems, which
  return `EINVAL`) -- a fallback triggered by a later read failure would silently turn a
  cache-defeating claim into a cache-poisoned one. `VerifyResult.Method` records which path was
  actually used (`unbuffered` vs `buffered_floor`) so this is inspectable, not just asserted.
  macOS `F_NOCACHE` / Windows `FILE_FLAG_NO_BUFFERING` (`verify_other.go`) are **not implemented
  in this PR** -- no macOS/Windows host was available to validate against, the same gap the plan
  doc's UI-stack section flags for tray packaging. The documented floor (`fsync` + close +
  reopen, no direct I/O) still applies on those platforms; this is a stated limitation; a
  follow-up PR adding either is additive.
- **pHash is computed from the just-written local edit copy, not the card.** Exif extraction
  (`internal/ingest.Exiftool.Exif`) still runs against the source path directly, before the
  copy -- it supplies the naming template's `{camera_model}`/`{yyyy}-{mm}-{dd}` placeholders, so
  it has to happen first, and it is its own necessary subprocess read regardless of what
  `DualWrite`'s single Go-level byte-stream pass does. pHash, by contrast, has no such ordering
  requirement and would mean a *second* full read of a possibly slow SD card reader if it ran
  against the source -- so it runs against the local destination copy after `Verify` has already
  proven that copy byte-identical to the source. "One read of the card," per issue #2, is
  specifically about not doubling the cost of the large sequential byte-stream pass; it does not
  (and structurally cannot) extend to exiftool's or pHash's own independent reads.
- **`.xmp`/`.srt` sidecar files are copied to both destinations but never get their own
  `EVENT_NODE_CREATED`.** The archive is meant to be a complete mirror of the card, so
  `internal/ingest.Engine` still runs them through `DualWrite`+`Verify`; `FileResult.Skipped` is
  what suppresses submission. This sidesteps branchDAM issue #249 (bare `.xmp` files becoming
  orphan graph nodes) for the agent path specifically, without deciding #249 itself -- a
  server-side scan of the same archive directory will still register the sidecar as its own
  `media_nodes` row via its existing `.xmp`-as-orphan behavior, which is why the parity test keys
  its file-by-file diff on the set of files the agent actually submitted, not on every row either
  database ends up with.
- **DJI `.srt` GPS lands on the video's own event, never a separate node or edge.** Ported
  `internal/djisrt.ParseFirstPoint` (`isValidFix` rejects the pre-lock `(0,0)` placeholder, same
  as branchDAM's own copy) is called only for a video-extension file with a same-stem `.srt`
  sidecar (`internal/ingest.findSRTSidecar`, tried against both `.srt` and `.SRT` casing); the
  first valid fix's lat/lon are set directly on that video's `NodeCreatedPayload`. This is what
  sidesteps branchDAM issue #251 (classifying `.srt` in general is unsafe, since it's also the
  universal subtitle extension) entirely -- there is no `.srt`-specific classification logic
  here, only a GPS lookup scoped to videos.

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
