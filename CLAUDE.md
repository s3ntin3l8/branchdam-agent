# CLAUDE.md — branchdam-agent

The workstation agent for [branchDAM](https://github.com/s3ntin3l8/branchdam), phase 10 of the
original spec. Scaffolded from
[`s3ntin3l8/go-http-template`](https://github.com/s3ntin3l8/go-http-template); the template's
HTTP-server scaffolding (`cmd/server/`, `internal/httpapi/`, `Dockerfile`) was stripped early on
because this is a desktop CLI/tray binary, not a service. If you are an AI agent or developer
working in this repo, read this first, then read branchDAM's
[`docs/roadmap.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/roadmap.md) and
[`docs/agent-protocol.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/agent-protocol.md)
for how this repo fits into the overall project and the wire contract it implements against.

## What this does

branchDAM's server side ships the full `/api/v1/agent/*` contract (event queue drainer,
handshake, path rebase) since its own phase 8; this repo is what speaks it from a workstation. It
ships as `v1.0.0`+ with everything below landed:

- `internal/branchdam/` -- the REST client (`hello`/`handshake`/`events`/`rebase`), with DTOs
  hand-synced to branchDAM's own `internal/agent/types.go`.
- Three byte-for-byte ported pieces of branchDAM server logic (`FastHash`, `PerceptualHash`'s
  call sequence, `naming.Stem`) an agent-ingested file has to reproduce exactly to stay
  consistent with a normal server-side scan.
- `preflight` -- checks server reachability/version, `exiftool` on `PATH`, and prints the
  configured path mappings.
- `ingest`/`internal/ingest/` -- the SD-card ingest core: poll-based card detection,
  one-read/two-write dual-copy writer, a cache-defeating verified re-read, DJI `.srt` telemetry
  parsing, metadata extraction at promoted-column parity with a server-side scan.
- `internal/tray/`, `internal/autostart/`, `internal/selfupdate/`, the `tray` subcommand -- the
  tray shell (windows/darwin), login-item registration, self-update wiring, all off by default
  except the tray icon itself. See [`docs/platform-support.md`](docs/platform-support.md) for the
  per-platform breakdown and known gaps.
- `internal/queue/`, `queue-drain` -- the offline queue: every intended event persisted to
  `queue.db` before any network call, so a workstation with no route to the NAS can still ingest,
  then finish the archive copy and rebase once reconnected. See
  [`docs/offline-queue.md`](docs/offline-queue.md).
- `hooks/resolve/` -- a DaVinci Resolve post-render hook (Python) that writes `.dam.json`.
- `internal/luminar/`, `internal/nodeindex/`, `luminar-sync` -- reads a Luminar `catalog.db` and
  emits `EVENT_EDGE_ATTACHED` for edit->source pairs it can resolve. Schema mapping is unverified
  against a real catalog -- see [`docs/luminar-catalog.md`](docs/luminar-catalog.md).
- `prune` -- deletes an offline-ingested file's local-edit-root mirror once the server confirms
  the Tier-3 archive copy is live and hash-verified. Not real Tier-1 NLE scratch pruning; see the
  "`prune` never trusts a single signal" invariant below.

The milestone-by-milestone history (M0 repo scaffold through M4 Luminar reader) is in
`CHANGELOG.md`, not repeated here -- this section describes what the code does today.

Why Go and not Rust/Tauri: `internal/hashing.PerceptualHash` has to be bit-identical to
branchDAM's own, which only holds if both sides call the same `goimagehash` library.

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
| `cmd/branchdam-agent/main.go` | Entrypoint + subcommand dispatch (`preflight`, `luminar-sync`, `ingest`, `tray`, `version`) |
| `cmd/branchdam-agent/preflight.go` | `preflight`'s checks (server reachability/version, `exiftool` on `PATH`, path mappings), factored out of `main.go` so it's testable without capturing stdout |
| `cmd/branchdam-agent/luminarsync.go` | `luminar-sync`'s flag parsing and orchestration (open catalog, load node index, run `luminar.Syncer`, print a summary); `--dump-schema` mode for recovering a real catalog's schema |
| `cmd/branchdam-agent/ingest.go` | `ingest --card <path> --config <path>`: the headless driver over `internal/ingest.Engine`, and the report printer that decides the process exit code / "safe to eject" line |
| `cmd/branchdam-agent/tray.go` | `tray -config <path>`: builds the same `ingest.Engine` `ingest.go` does, wires `internal/tray.Runner`, the status server, optional self-update check, and optional login-item registration, then calls `tray.Run` (blocks until the tray quits or the process is signalled) |
| `cmd/branchdam-agent/prune.go` | `prune -config <path> [-dry-run] [-watch]` (branchdam#230-adjacent): deletes an offline-ingested file's `LocalEditRoot` mirror once `POST /api/v1/agent/node-status` confirms the Tier-3 archive copy live and hash-verified. Only ever considers `queue.db` rows with `Record.Done() == true`; see the "Prune never trusts..." invariant below |
| `internal/branchdam/` | The REST client for branchDAM's `/api/v1/agent/*` contract -- one file per endpoint (`hello.go`, `handshake.go`, `events.go`, `rebase.go`), `types.go` for the hand-synced DTOs, `errors.go` for client-side validation gates and fatal/transient error classification, `conformance_test.go` + `testdata/*.golden.json` for the byte-for-byte fixture tests |
| `internal/hashing/` | Byte-for-byte port of branchDAM's `FastHash` (sampled xxHash64) and `PerceptualHash` (thin `goimagehash.PerceptionHash` wrapper), plus M1's own addition `StreamingFastHasher` -- a single-pass, `io.Writer`-shaped variant proven byte-identical to `FastHash` by an equivalence test, so the M1 dual-copy writer never re-reads the card a second time just to compute `fast_hash` |
| `internal/naming/` | Byte-for-byte port of branchDAM's `naming.Stem`/`naming.Analyze` filename normalization |
| `internal/phash/` | Port of branchDAM's `probe.ExtractPHash` *call sequence* (direct decode, then exiftool `-PreviewImage`/`-JpgFromRaw`/`-ThumbnailImage` fallback, first decodable wins) -- not a reimplementation of exiftool itself |
| `internal/luminar/` | Reads a Luminar `catalog.db` (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` for edit->source pairs; the actual (unverified) schema query is isolated in `query.go` and overridable via `--query-file` -- see `docs/luminar-catalog.md` |
| `internal/nodeindex/` | Maps a file path to the `nodeUuid` it was ingested as (`Resolver` interface, `FileIndex` JSON-file implementation) -- works around there being no agent-reachable lookup-by-path endpoint on branchDAM |
| `internal/djisrt/` | Byte-for-byte port of branchDAM's `internal/djisrt` -- DJI `.srt` first-GPS-fix telemetry parser, including `isValidFix`'s pre-lock `(0,0)` rejection |
| `internal/ingest/` | M1's SD-card ingest core, UI-free: `carddetect.go` (poll-based removable-volume detection), `writer.go` (one-read/two-write `DualWrite`), `verify.go`+`verify_linux.go`+`verify_other.go` (cache-defeating re-read, O_DIRECT on Linux with a documented buffered-floor fallback), `metadata.go` (exiftool extraction ported from branchDAM's `probe.go`, sidecar merge, `capturedAt` fallback chain), `srt.go` (DJI GPS-on-video wiring over `internal/djisrt`), `naming.go` (destination path template), `pathmap.go` (workstation->container path translation), `extensions.go` (video/image extension sets, copied from branchDAM's `pipeline.videoExts`/`imageExts` for parity), `ingest.go` (`Engine`, the orchestrator both the CLI and a later tray drive) |
| `internal/tray/` | The tray shell (issue #3): `tray.go` (`Runner` -- watch-dir/scratch/queue-stub state and `TriggerIngest`, no UI import, unit-tested on any host), `run_supported.go` (`//go:build windows \|\| darwin`, the repo's only `fyne.io/systray` import: menu wiring, card-insertion auto-ingest via `internal/ingest.Detector`), `run_unsupported.go` (`//go:build !windows && !darwin`, returns `ErrUnsupported` -- this is what Linux CI actually builds/tests), `statusserver.go` + `assets/index.html` (embedded `net/http` status page, loopback-only by construction -- see `normalizeLoopback`), `icon.go` (`buildTrayIcon` renders the tray icon in Go at startup -- a PNG wrapped as a single-image `.ico` container, no binary asset committed to the repo; a single `.ico` buffer works for both windows and darwin per `fyne.io/systray`'s own doc comment) |
| `internal/autostart/` | Login-item registration, off by default (`tray.startOnLogin`): `autostart.go` (untagged plist-XML rendering, unit-tested on Linux), `autostart_darwin.go` (`//go:build darwin`, writes + `launchctl load`s a LaunchAgent plist), `autostart_windows.go` (`//go:build windows`, `golang.org/x/sys/windows/registry` write to `HKCU\...\Run`), `autostart_other.go` (`//go:build !windows && !darwin`, `ErrUnsupported` stub) |
| `internal/selfupdate/` | Thin wrapper over `github.com/creativeprojects/go-selfupdate` v1.6.0's `DetectLatest`/`UpdateCommand`, gated by `selfUpdate.enabled` (off by default) -- `selfupdate.go` (the implementation, compiled into every build), `result.go` (`CheckResult`) |
| `internal/config/` | YAML config loader: branchDAM server URL + API key, this workstation's self-asserted `agentId`, the workstation-path -> container-path map `preflight` prints, `ingest:` -- archive/local-edit roots, the naming template, and card-detection polling -- `tray:`/`selfUpdate:`, and `prune:` (`PruneConfig`: `enabled`, `minAgeHours`, opt-in and off by default) |
| `config.example.yaml` | Reference config with `${VAR}` placeholders |
| `.github/workflows/` | Thin callers of the reusable workflows in `s3ntin3l8/.github`, minus the Docker jobs the template ships (no image is published from this repo), plus this repo's own `build-windows`/`build-darwin`/`build-darwin-full` jobs in `ci-cd.yml` (not covered by the shared `ci-go.yml`, which only builds for the runner's own host OS/arch) |
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
  refuses to overwrite an existing destination (`O_EXCL`) -- on verification failure, `internal/ingest.Engine`
  cleans up both partial destination copies so subsequent retries never wedge on "file exists". Destination
  naming collisions are resolved automatically (`ResolveDestination`): identical existing destination files are
  skipped gracefully, while distinct files with the same rendered path are auto-suffixed (`_2`, `_3`) with
  matching sidecar pairing.
- **Verify decides unbuffered-vs-buffered-floor once, at open time, and never falls back mid-stream.**
  `internal/ingest.Verify` re-reads a file DualWrite already `fsync`'d and closed; on Linux
  (`verify_linux.go`) it tries `O_DIRECT`, on macOS (`verify_darwin.go`) it uses `F_NOCACHE` (fcntl),
  and on Windows (`verify_windows.go`) it uses `FILE_FLAG_NO_BUFFERING` via `CreateFile` with sector-aligned
  reads. Each platform falls back to the plain reopen floor only if the unbuffered open itself fails (the
  expected case on tmpfs, network shares, or filesystems lacking direct I/O support) -- a fallback
  triggered by a later read failure would silently turn a cache-defeating claim into a cache-poisoned one.
  `VerifyResult.Method` records which path was actually used (`unbuffered` vs `buffered_floor`). The
  optional config knob `ingest.requireUnbuffered` (default `false`) treats any degradation to
  `buffered_floor` as a fatal verification failure, withholding safe-eject.
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
- **The tray shell (`internal/tray/`) never duplicates `internal/ingest.Engine`'s logic --
  `Runner.TriggerIngest` is the one place both the menu-click handler and the card-insertion
  watch loop call into it.** Both `cmd/branchdam-agent/tray.go` and `ingest.go` build the same
  `ingest.NewEngine(...)` and hand it to their respective driver; a change to ingest behavior
  never needs a matching tray-side change.
- **The status page binds loopback-only by construction, not by convention.**
  `tray.normalizeLoopback` rewrites a bare `":port"` (which `net/http` would otherwise bind to
  every interface) to `"127.0.0.1:port"` before `ListenAndServe` ever runs -- CodeQL is a required
  check and a wide-open bind on a page that renders local filesystem paths (and, after M2, queue
  depth) is exactly the kind of thing it flags. The page also never renders `server.apiKey`;
  `TestHandleIndexRendersStatus` pins both (loopback rewrite + no-secret-leak) as regression
  tests.
- **Queue status is a literal stub string (`tray.QueueStatusStub`) until M2, never a fabricated
  number.** Same "no fake numbers" discipline the M1 gate's step 2b documents for
  `node_metadata` counts -- issue #3 says this explicitly, and `TestStatusQueueStatusIsAlwaysTheStub`
  is the regression test.
- **`-H windowsgui` is link-scoped to the whole binary, so there are two Windows build outputs
  from the same `cmd/branchdam-agent` source, not one.** `branchdam-agent.exe` (console-linked)
  serves `preflight`/`ingest`/`luminar-sync`, whose stdout an operator needs; `branchdam-agent-tray.exe`
  (`-H windowsgui`-linked) is for the tray/login-item launch path, where a console flash on every
  start would be the exact bug issue #3 calls out. Both come from `make build-windows`.
- **`internal/tray`'s systray import is isolated behind `//go:build windows || darwin`
  (`run_supported.go`), with a `run_unsupported.go` stub (`//go:build !windows && !darwin`) for
  everything else.** This is not defensive boilerplate -- `fyne.io/systray` v1.12.2's darwin
  backend needs cgo (Objective-C) and fails to even compile under `CGO_ENABLED=0`
  (`undefined: setInternalLoop` etc), and there is no darwin cgo cross-toolchain on the
  `ubuntu-latest` runner this repo's required `test-go`/`golangci-lint` checks use. Without the
  split, importing `internal/tray` from `cmd/branchdam-agent` would either force cgo everywhere or
  break Linux CI outright. Verified locally before writing any tray code, not assumed from the
  plan doc's Windows/darwin framing (which says nothing about Linux).
- **Both login-item registration and self-update are off by default, and stay off unless the
  operator sets the corresponding config flag.** `tray.startOnLogin: false` and
  `selfUpdate.enabled: false` in `config.example.yaml`; `cmd/branchdam-agent/tray.go` reads both
  directly off `config.Config` with no separate CLI override that could silently re-enable either.
- **`internal/selfupdate`'s real implementation is compiled into every build, no build tag
  required.** `go-selfupdate` v1.6.0's top-level package (`validate.go`) imports
  `golang.org/x/crypto/openpgp` unconditionally (regardless of which `Validator` you actually
  configure), which is `GO-2026-5932` -- "unmaintained, unsafe by design ... Fixed in: N/A" -- and
  v1.6.0 is `go-selfupdate`'s newest tag, so there is no upgrade path either. This repo's
  **required** `test-go / lint-and-test` check suppresses that specific finding via
  `s3ntin3l8/.github/ci-go.yml`'s `govulncheck-ignore` input (`ci-cd.yml`'s `test-go` job passes
  `govulncheck-ignore: "GO-2026-5932"`) -- see `s3ntin3l8/.github#49` for that shared-workflow
  change and issue #14 for the history (this repo previously worked around the lack of that input
  with a `-tags selfupdate` build-tag split; that split is gone now that the input exists). Drop
  the `govulncheck-ignore` entry once `go-selfupdate` itself stops importing `openpgp`
  unconditionally.
- **Binary publishing is chained inside the release-please run, gated on `release_created`,
  never triggered by `on: release`.** release-please creates the GitHub Release with the default
  `GITHUB_TOKEN`, and GitHub's Actions recursion guard means a `GITHUB_TOKEN`-created release
  does not fire `on: release` -- `v1.0.0` shipped with zero downloadable assets for exactly this
  reason (its `release-binaries.yml` had only ever run via manual `workflow_dispatch`).
  `release-please.yml`'s `binaries` job now calls `release-binaries.yml` as a `workflow_call`,
  fed `tag_name` from `release-please`'s own output, the same pattern branchDAM's own
  `release.yml` uses to chain its image push. A reusable-workflow call's permissions come from
  the *caller* job, so `contents: write` lives on `release-please.yml`'s `binaries:` job, not
  (only) inside `release-binaries.yml` itself.
- **`prune` never trusts a single signal before deleting a file -- server verification, a
  containment check, and a TOCTOU re-stat all have to agree.** `Record.Done()` alone means
  nothing about server-side truth (a permanently `RebaseStatus=FAILED` row still passes it);
  the actual gate is `POST /api/v1/agent/node-status` reporting `found && verified && tier ==
  TIER3_MASTER_ARCHIVE && lifecycleState in (ACTIVE, HIDDEN)`. Even then, `withinRoot`
  (`filepath.EvalSymlinks` on both `LocalEditRoot` and the candidate, never a lexical
  `filepath.Rel`/`HasPrefix` check) must confirm the resolved path is actually under
  `LocalEditRoot` -- this agent has no `storage.Guard` equivalent, so this check *is* that
  safety net. And the file is re-`os.Lstat`'d immediately before deletion and compared against
  `Record.SizeBytes`/`MtimeUnix`, since the node-status round trip is real elapsed time during
  which the file could change. Sidecar (`queue.KindSidecar`) rows are never looked up at all --
  they never get their own `EVENT_NODE_CREATED`, so their `NodeUUID` can never resolve
  server-side. This is branchdam#230-adjacent, not a fix for it: only an offline-ingested file's
  own `LocalEditRoot` mirror is ever prunable, never real Tier-1 `LOCAL_SCRATCH` (Resolve
  caches/proxies) -- see branchDAM's `docs/workflow-coverage.md` item 12 for why that stays
  architecturally blocked.

## CI/CD — uses centralized reusable workflows

Workflows here are **callers** of `s3ntin3l8/.github/.github/workflows/*.yml@main`: `ci-cd.yml`
(`ci-go` only -- no `build-docker`, unlike the template), `codeql.yml`, `dependency-review.yml`,
`release-please.yml` (release-please, no Docker publish step, plus this repo's own chained
`binaries` job -- see below and the recursion-guard invariant above).

**`release-please.yml` -> `release-binaries.yml`.** `release-please.yml` has two jobs:
`release-please` (the shared reusable workflow) and `binaries`, gated on
`needs.release-please.outputs.release_created == 'true'`, which calls
`./.github/workflows/release-binaries.yml` as a `workflow_call` with `release-tag` set to
`release-please`'s `tag_name` output. `release-binaries.yml` itself builds all three platforms
(see "Layout"/`.github/workflows/`) and, in its `upload` job (gated on `inputs.release-tag !=
''`, not on the trigger event), attaches the per-platform archives plus a checksums file to the
GitHub Release. `workflow_dispatch` on `release-binaries.yml` still works standalone, for a
build-only smoke test (empty `release-tag`) or to backfill assets onto an existing release.

`ci-cd.yml` also has three jobs of its own (not from the shared `ci-go.yml`, which only builds for
the runner's own host OS/arch), added for the tray shell (issue #3): **`build-windows`**
(`ubuntu-latest`, `CGO_ENABLED=0 GOOS=windows`, pure Go for `fyne.io/systray` on that platform --
builds both `dist/branchdam-agent.exe` and the `-H windowsgui`-linked
`dist/branchdam-agent-tray.exe` via `make build-windows`); **`build-darwin`** (`ubuntu-latest`,
`CGO_ENABLED=0 GOOS=darwin`, build-only, deliberately excluding `internal/tray` and
`cmd/branchdam-agent` -- `fyne.io/systray`'s darwin backend is cgo/Objective-C and there is no
darwin cgo cross-toolchain on `ubuntu-latest`, so this only proves the rest of the module still
cross-compiles clean); and **`build-darwin-full`** (`runs-on: macos-26`, `timeout-minutes: 10`,
builds the whole module including the tray package -- whether this job is usable at all depends
on `macos-26` runner availability for this public repo, checked empirically rather than assumed;
see the PR that introduced it for the result). None of the three is in branch protection's
required-checks list.

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

- `README.md` -- setup, install, and usage instructions.
- `CLAUDE.md` (this file) -- the live contract for how the repo is wired: layout, invariants,
  CI/CD conventions. Keep it in sync with the code, not with what the code used to do.
- [`docs/platform-support.md`](docs/platform-support.md) -- per-platform support matrix (tray vs.
  headless, why Windows ships two binaries, the `fyne.io/systray` cross-compile matrix) and known
  gaps (macOS `.app` bundle, tray queue-status stub, unvalidated Luminar schema).
- [`docs/offline-queue.md`](docs/offline-queue.md) -- the offline queue's state machine and the
  server-side prerequisite it depends on.
- [`docs/luminar-catalog.md`](docs/luminar-catalog.md) -- the Luminar `catalog.db` schema
  research, confidence level, and how to correct the query against a real catalog.
- branchDAM's [`docs/roadmap.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/roadmap.md)
  and [`docs/agent-protocol.md`](https://github.com/s3ntin3l8/branchdam/blob/main/docs/agent-protocol.md)
  -- how this repo fits into the overall project and the wire contract it implements against;
  the source of truth for anything not covered above.
