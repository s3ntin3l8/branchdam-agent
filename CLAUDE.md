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
- `internal/tray/`, `internal/autostart/`, `internal/selfupdate/`, `internal/appbundle/`, the
  `tray` and `update` subcommands -- the tray shell (windows/darwin), login-item registration
  (off by default), checksum-verified self-update (checking on by default -- a read-only GitHub
  API call; applying always requires explicit confirmation: a tray menu click or the headless
  `update` subcommand), the macOS `.app` bundle. See
  [`docs/platform-support.md`](docs/platform-support.md) for the per-platform breakdown and known
  gaps.
- `internal/queue/`, `queue-drain` -- the offline queue: every intended event persisted to
  `queue.db` before any network call, so a workstation with no route to the NAS can still ingest,
  then finish the archive copy and rebase once reconnected. `Store.Counts` is a single aggregate
  query (never `len(Pending())` or bucketing `All()` in Go) for a cheap live readout over a large
  backlog, reporting a permanently-`FAILED` rebase separately from genuinely `Done` rows on
  purpose -- see [`docs/offline-queue.md`](docs/offline-queue.md).
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
| `cmd/branchdam-agent/main.go` | Entrypoint + subcommand dispatch (`preflight`, `luminar-sync`, `ingest`, `tray`, `update`, `init`, `dialog` (hidden, see below), `version`) |
| `cmd/branchdam-agent/preflight.go` | `preflight`'s checks (server reachability/version, `exiftool` on `PATH`, path mappings, `config.Validate()`'s problems), factored out of `main.go` so it's testable without capturing stdout |
| `cmd/branchdam-agent/luminarsync.go` | `luminar-sync`'s flag parsing and orchestration (open catalog, load node index, run `luminar.Syncer`, print a summary); `--dump-schema` mode for recovering a real catalog's schema |
| `cmd/branchdam-agent/ingest.go` | `ingest --card <path> --config <path>`: the headless driver over `internal/ingest.Engine`, and the report printer that decides the process exit code / "safe to eject" line |
| `cmd/branchdam-agent/init.go` | `init [-config <path>] [-force]` (issue #30): writes the embedded starter config (`assets/config.starter.yaml`, literal empty required fields -- never `config.example.yaml`, whose `${VAR}` placeholders would immediately trip `Validate()`) to `config.ResolvePath`'s resolved location; refuses to overwrite an existing file without `-force` |
| `cmd/branchdam-agent/dialog.go` | The hidden `dialog -kind <error\|entry\|password\|directory>` subcommand (issue #30) -- omitted from `usage()`, meant only to be re-exec'd by this same binary, never invoked directly. Wraps `github.com/ncruces/zenity` (no cgo on any platform; Win32-native on Windows, `osascript` on macOS, the `zenity`/`matedialog`/`qarma` binary on Linux) behind an injectable `dialogFuncs`, so its flag parsing and exit-code contract (`dialogExitOK`/`Failed`/`Canceled`) are unit-tested without a real display |
| `cmd/branchdam-agent/bootstrap.go` | `notifyStartupFailure` (a best-effort error dialog naming the log path) and `bootstrapConfigInteractive` (the first-run setup wizard: server URL, API key, the two ingest roots, applied via `config.Patch`) -- both re-exec `dialog` through an injectable `dialogRunner` rather than calling zenity in-process; see the tray invariant below for why |
| `cmd/branchdam-agent/tray.go` | `tray -config <path>`: resolves a `dialogRunner` and `selfExe` up front (`trayDialogSetup`), sets up `internal/agentlog`, runs first-run bootstrap on a missing config, builds the same `ingest.Engine` `ingest.go` does, wires `internal/tray.Runner`, a `configSettings` (`settings.go`), opens `queue.db` and wires `Runner.SetQueueDeps` plus the drain/prune background timers when `offline.queueDbPath` is set (issue #32, see `queueagent.go`), the status server (`Listen()` before `tray.Run` starts, as the tray's single-instance guard), `selfUpdateAgent`, and optional login-item registration, then calls `tray.Run` and, if it returns `Outcome.RestartRequested`, `relaunchSelf` once the status listener is confirmed released -- `Outcome.AppliedVersion` empty distinguishes a settings-driven restart from a self-update one for the log line. Every failure path goes through a local `fail()` closure that logs and shows a best-effort dialog -- see the startup-diagnostics invariant below |
| `cmd/branchdam-agent/settings.go` | `configSettings` (issue #31): implements `tray.Settings` over `config.Patch`/`config.Load` plus a `dialogRunner` for the five free-text fields (server URL, API key, the two ingest roots, naming template). `SetBool`/`SetInt`/`PromptAndSet` all funnel through one `reload()` that rebuilds `branchdam.Client`+`ingest.Engine` and applies them via `Runner.Reconfigure`, and re-derives `SettingsView.RestartRequired` by diffing `tray.statusAddr`/`ingest.cardRoots` against the previous snapshot on every call -- so a hand-edited config picked up by "Reload config" is caught exactly like a menu-driven change. `tray.startOnLogin`'s `SetBool` call additionally drives `internal/autostart.Enable`/`Disable` directly, best-effort |
| `cmd/branchdam-agent/selfupdateagent.go` | `selfUpdateAgent`: implements `tray.SelfUpdater` over a configured `internal/selfupdate.Updater` -- the initial check plus a periodic re-check (`selfUpdate.checkIntervalHours`), stopping permanently once a check reports `ErrVersionNotSemver` |
| `cmd/branchdam-agent/relaunch.go` | `relaunchSelf`: spawn-then-exit restart after a successful self-update, on every platform (never `syscall.Exec` -- see the "relaunch happens after..." invariant below); detects a macOS `.app` bundle and restarts it via `open -n -a` instead of a bare `exec.Command` |
| `cmd/branchdam-agent/update.go` | `update -config <path> [-check] [-yes]`: the headless equivalent of the tray's "Install and restart" menu item, gated on `selfUpdate.enabled` exactly like the tray's background check |
| `cmd/branchdam-agent/prune.go` | `prune -config <path> [-dry-run] [-watch]` (branchdam#230-adjacent): flag parsing plus the config/queue/client wiring `internal/prune.Pass` needs -- the actual pass logic was extracted there so the tray (a later PR) can call it without importing `package main` |
| `cmd/branchdam-agent/queueagent.go` | Issue #32's concrete wiring: `queueCountsReader`/`queueDrainer`/`queuePruner` implement `tray.QueueReader`/`Drainer`/`Pruner` over a real `*queue.Store`+`*branchdam.Client` (`queuePruner.Prune` always runs non-dry-run, discarding `internal/prune.Pass`'s per-file lines to `io.Discard` since there's no console for a `-H windowsgui` tray to write them to -- `Stats` is what the menu/status page show instead), plus `startPeriodic` (mirrors `selfUpdateAgent.Run`'s own ticker shape) -- the generic ticker loop `tray.go` starts twice, once for `offline.drainIntervalSecs` and once for `prune.intervalMinutes`, entirely decoupled from `internal/tray/run_supported.go`'s menu-refresh select loop |
| `internal/prune/` | `Pass` (deletes an offline-ingested file's `LocalEditRoot` mirror once `POST /api/v1/agent/node-status` confirms the Tier-3 archive copy live and hash-verified; only ever considers `queue.db` rows with `Record.Done() == true`), `Stats`, `PrintReport`, and the unexported `withinRoot` containment check -- extracted from `cmd/branchdam-agent/prune.go` as a pure move, zero behavior change; see the "Prune never trusts..." invariant below |
| `internal/branchdam/` | The REST client for branchDAM's `/api/v1/agent/*` contract -- one file per endpoint (`hello.go`, `handshake.go`, `events.go`, `rebase.go`), `types.go` for the hand-synced DTOs, `errors.go` for client-side validation gates and fatal/transient error classification, `conformance_test.go` + `testdata/*.golden.json` for the byte-for-byte fixture tests |
| `internal/hashing/` | Byte-for-byte port of branchDAM's `FastHash` (sampled xxHash64) and `PerceptualHash` (thin `goimagehash.PerceptionHash` wrapper), plus M1's own addition `StreamingFastHasher` -- a single-pass, `io.Writer`-shaped variant proven byte-identical to `FastHash` by an equivalence test, so the M1 dual-copy writer never re-reads the card a second time just to compute `fast_hash` |
| `internal/naming/` | Byte-for-byte port of branchDAM's `naming.Stem`/`naming.Analyze` filename normalization |
| `internal/phash/` | Port of branchDAM's `probe.ExtractPHash` *call sequence* (direct decode, then exiftool `-PreviewImage`/`-JpgFromRaw`/`-ThumbnailImage` fallback, first decodable wins) -- not a reimplementation of exiftool itself |
| `internal/luminar/` | Reads a Luminar `catalog.db` (`?mode=ro`, never `?immutable=1`) and emits `EVENT_EDGE_ATTACHED` for edit->source pairs; the actual (unverified) schema query is isolated in `query.go` and overridable via `--query-file` -- see `docs/luminar-catalog.md` |
| `internal/nodeindex/` | Maps a file path to the `nodeUuid` it was ingested as (`Resolver` interface, `FileIndex` JSON-file implementation) -- works around there being no agent-reachable lookup-by-path endpoint on branchDAM |
| `internal/djisrt/` | Byte-for-byte port of branchDAM's `internal/djisrt` -- DJI `.srt` first-GPS-fix telemetry parser, including `isValidFix`'s pre-lock `(0,0)` rejection |
| `internal/ingest/` | M1's SD-card ingest core, UI-free: `carddetect.go` (poll-based removable-volume detection), `writer.go` (one-read/two-write `DualWrite`), `verify.go`+`verify_linux.go`+`verify_other.go` (cache-defeating re-read, O_DIRECT on Linux with a documented buffered-floor fallback), `metadata.go` (exiftool extraction ported from branchDAM's `probe.go`, sidecar merge, `capturedAt` fallback chain), `srt.go` (DJI GPS-on-video wiring over `internal/djisrt`), `naming.go` (destination path template), `pathmap.go` (workstation->container path translation), `extensions.go` (video/image extension sets, copied from branchDAM's `pipeline.videoExts`/`imageExts` for parity), `ingest.go` (`Engine`, the orchestrator both the CLI and a later tray drive), `progress.go` (`ProgressEvent`/`WriteOption`/`WithProgress`/`DrainOption`/`WithDrainProgress` -- optional byte-progress reporting threaded into `DualWrite`/`WriteLocal`/`CopyToArchive`/`Verify` and `Drain`'s archive-copy phase via variadic options, so every existing call site keeps compiling unchanged; `Engine.Progress`, nil by default, is what a live tray readout wires up) |
| `internal/tray/` | The tray shell (issue #3): `tray.go` (`Runner` -- ingester/watch-dir/scratch/queue state all guarded by one `mu` now that `Reconfigure` can swap the ingest fields at runtime, `TriggerIngest` serialized behind `gate`, `Busy`/`TryLockIdle` for the self-update gate, `Reconfigure` for issue #31's guarded rebuild, `TriggerDrain`/`TriggerPrune` for issue #32 (see the drain/prune locking invariant below), `SelfUpdater`/`UpdateStatus`, no UI import, unit-tested on any host), `queue.go` (issue #32: `QueueCounts`/`QueueReader`, `DrainSummary`/`Drainer`, `PruneSummary`/`Pruner`, `QueueStatus` -- tray-local mirrors of `internal/queue.Counts`/`internal/ingest.DrainStats`/`internal/prune.Stats`, kept separate so `internal/tray` never imports those packages directly, implemented in `cmd/branchdam-agent/queueagent.go`), `settings.go` (the platform-independent `Settings` interface + `SettingsView`/`SettingsField` -- defined here, implemented in `cmd/branchdam-agent/settings.go`, matching `Ingester`/`SelfUpdater`'s existing split), `run_supported.go` (`//go:build windows \|\| darwin`, the repo's only `fyne.io/systray` import: menu wiring including "Install and restart", "Drain queue now"/"Prune now", and the "Settings" submenu, card-insertion auto-ingest via `internal/ingest.Detector`, returns `Outcome`), `settingsmenu.go` (`//go:build windows \|\| darwin`, the "Settings" submenu's own systray items and click-dispatch goroutine, kept out of `run_supported.go`'s already-large `Run` function), `run_unsupported.go` (`//go:build !windows && !darwin`, returns `ErrUnsupported` -- this is what Linux CI actually builds/tests), `statusserver.go` + `assets/index.html` (embedded `net/http` status page, loopback-only by construction -- see `normalizeLoopback`; `Listen()`/`Serve()` split so the bind doubles as a single-instance guard), `icon.go` (`buildTrayIcon` renders the tray icon in Go at startup -- a PNG wrapped as a single-image `.ico` container, no binary asset committed to the repo; a single `.ico` buffer works for both windows and darwin per `fyne.io/systray`'s own doc comment) |
| `internal/autostart/` | Login-item registration, off by default (`tray.startOnLogin`): `autostart.go` (untagged plist-XML rendering, unit-tested on Linux), `autostart_darwin.go` (`//go:build darwin`, writes + `launchctl load`s a LaunchAgent plist), `autostart_windows.go` (`//go:build windows`, `golang.org/x/sys/windows/registry` write to `HKCU\...\Run`), `autostart_other.go` (`//go:build !windows && !darwin`, `ErrUnsupported` stub) |
| `internal/selfupdate/` | Wraps `github.com/creativeprojects/go-selfupdate` v1.6.0 behind a client that always sets a `ChecksumValidator` (`selfupdate.go`'s `Updater`, `Check`/`Apply`), refuses a non-semver running version or a non-newer release before any download (`errors.go`), and resolves what one `Apply` call replaces -- a Windows sibling `.exe`, a macOS bundle's `Info.plist` -- via `InstallLayout`/`DetectLayout`/`BundlePath` (`install.go`). Gated by `selfUpdate.enabled` (checking on by default, applying always a separate explicit action); compiled into every build, no build tag |
| `internal/appbundle/` | Renders and assembles the macOS `.app` bundle (`RenderInfoPlist`, `BundleVersion`, `Write`) -- shared by `tools/mkbundle` (build time) and `internal/selfupdate.Apply` (update time, rewriting `Info.plist` after a binary-only swap), so the two can't drift into rendering different plists |
| `tools/mkbundle/` | CLI wrapper over `internal/appbundle.Write`, used by `make build-darwin-app` and `release-binaries.yml`'s `build-darwin` job |
| `internal/config/` | YAML config loader: branchDAM server URL + API key, this workstation's self-asserted `agentId`, the workstation-path -> container-path map `preflight` prints, `ingest:` -- archive/local-edit roots, the naming template, and card-detection polling -- `offline:`'s `drainIntervalSecs` (the tray's own drain-timer period, issue #32), `tray:`/`selfUpdate:` (`Enabled`, `Repo`, `CheckIntervalHours`), and `prune:` (`PruneConfig`: `enabled`, `minAgeHours`, `intervalMinutes` (the tray's own prune-timer period, issue #32), opt-in and off by default). `config.go`'s `DefaultPath`/`ResolvePath` give every subcommand a real fallback config location (`os.UserConfigDir()/branchdam-agent/config.yaml`) instead of a CWD-relative `"config.yaml"` default, and `Validate() []Problem` centralizes the checks that apply regardless of which subcommand is running -- most importantly catching an unset `${VAR}` left as a literal placeholder (see the `expandEnv` invariant below), which otherwise passes a naive `!= ""` check and fails downstream looking like a server problem. `patch.go`'s `Patch(path, changes map[string]any)` is the only supported way to write a config change back to disk -- see the write-back invariant below |
| `config.example.yaml` | Reference config with `${VAR}` placeholders |
| `internal/agentlog/` | The agent's one shared logging setup: `Setup()` installs an `slog.Logger` writing to both stderr and a durable per-OS log file (`%LOCALAPPDATA%\branchDAM\logs\agent.log`, `~/Library/Logs/branchDAM/agent.log`, `$XDG_STATE_HOME/branchdam-agent/agent.log`) as the process-wide default, rotating a >5MB file to `.1` first; `SlogBridge` adapts it to the classic `Print`/`Printf` shape `github.com/creativeprojects/go-selfupdate`'s `Logger` interface expects. Not wired into every subcommand's entry point -- see its own package doc comment for why (`go test ./...` must never write outside a test's own `t.TempDir()`) |
| `.github/workflows/` | Thin callers of the reusable workflows in `s3ntin3l8/.github`, minus the Docker jobs the template ships (no image is published from this repo), plus this repo's own `build-windows`/`build-darwin`/`build-darwin-full` jobs in `ci-cd.yml` (not covered by the shared `ci-go.yml`, which only builds for the runner's own host OS/arch) and `hermes.yml` (automated PR review, see below) |
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
- **Config is written back surgically via a `yaml.Node` tree, never a `yaml.Marshal(cfg)`
  round-trip.** `Load` expands `${VAR}` into an in-memory `Config`, so marshaling that value back
  to disk would bake the *resolved* secret into `server.apiKey` in plaintext and destroy every
  comment in the file. `internal/config.Patch` (`patch.go`) instead parses the raw, un-expanded
  file into a `yaml.Node`, mutates only the scalar nodes named by its dotted-key `changes` map, and
  re-encodes -- every untouched `${VAR}` placeholder and every comment survives, pinned by
  `TestPatchPreservesCommentsAndUnexpandedPlaceholders`'s golden test against
  `config.example.yaml` itself. Written atomically (temp file, mode `0600`, then rename) since a
  tray settings menu writing `server.apiKey` from a dialog means a real secret can land on disk in
  plaintext for the first time.
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
  never needs a matching tray-side change. `TriggerIngest` also *serializes* those two callers
  (holds `Runner.gate` for its whole duration) -- nothing did before self-update needed a gate to
  hold, and two concurrent `IngestCard` runs over the same card root would otherwise race
  `internal/ingest`'s destination-collision resolution.
- **Settings changes apply through one guarded-rebuild mechanism, `Runner.Reconfigure`, never a
  per-field hot-patch path.** `SetBool`/`SetInt`/`PromptAndSet` (`cmd/branchdam-agent/settings.go`)
  all funnel through the same `reload()`: re-read `config.yaml`, rebuild `branchdam.Client` +
  `ingest.Engine`, swap them into the `Runner` via `Reconfigure`, which blocks on the same `gate`
  `TriggerIngest` holds so a config change can never land mid-copy. Exactly two fields are
  deliberately **not** reconfigurable this way and are tracked as `SettingsView.RestartRequired`
  instead: `tray.statusAddr` (its `Listen()` call already happened and is the tray's
  single-instance guard -- there's nothing to swap it into) and `ingest.cardRoots`
  (`internal/ingest.Detector`'s watch goroutine is a one-shot call over the roots it started with,
  not restartable from inside `tray.Run`'s select loop). `RestartRequired` is re-derived by
  diffing those two fields against the previous snapshot on *every* reload, not tracked per
  changed key -- so a hand-edited `config.yaml` picked up by "Reload config" is caught exactly
  the same way a menu-driven change is. A "Restart now" menu click reuses `Outcome.RestartRequested`
  (empty `AppliedVersion` distinguishes it from a self-update restart) and the same
  `relaunchSelf` path self-update already uses.
- **`Runner`'s `ingester`/`watchDirs`/`scratchDir` fields are unexported and guarded by the same
  `mu` as `last`/`busy`/`busyCard`/`busySince`.** Before issue #31's `Reconfigure`, these were set
  once at construction and read without synchronization everywhere (`Status`, `run_supported.go`'s
  watch-dir rendering and its "Ingest now" worker) -- safe only because nothing ever wrote them
  again. `Reconfigure` introduces a second writer, so every read site now goes through a
  lock-guarded accessor (`Runner.WatchDirs()`) or the existing `Status()` snapshot, not the bare
  field.
- **Every dialog the tray shows (a startup-error notification, the first-run setup wizard) renders
  in a re-exec'd `dialog` subprocess, never by calling `github.com/ncruces/zenity` in-process from
  `runTrayCmd`.** This isolates two platform-specific unknowns that couldn't be verified from this
  repo's Linux-only development/CI environment: whether a Win32 dialog renders correctly from a
  `-H windowsgui`-linked process before systray's own message pump has started, and whatever
  process-state assumptions a macOS `.app` launched by launchd carries. `dialogRunner` (a func
  type, `cmd/branchdam-agent/dialog.go`) is the indirection every caller (`notifyStartupFailure`,
  `bootstrapConfigInteractive`) goes through -- both are unit-tested against a fake runner, since
  neither the dialog rendering itself nor the re-exec plumbing can be exercised on Linux CI. See
  `docs/platform-support.md`'s Known gaps for what's unverified on real hardware as a result.
- **A missing config is no longer fatal for `tray` -- it triggers first-run bootstrap, not exit
  1.** `writeStarterConfig` (shared with `init`) plus a short zenity wizard collect
  `server.baseUrl`/`apiKey`/`ingest.archiveRoot`/`localEditRoot`, **and** a `pathMappings` entry
  (the wizard's fifth prompt) applied via `config.Patch` in one call. The `pathMappings` prompt
  exists because the tray fails fast on an empty `pathMappings` the same way it does on an empty
  ingest root -- without it, a completed wizard would launch a tray whose first real card ingest
  fails deep inside `internal/ingest` with a confusing `ErrNoPathMapping` instead of at startup.
  The starter config is left on disk even if the wizard is canceled or a dialog fails partway
  (`errBootstrapCanceled` vs. any other error) -- there is always something to hand-edit
  afterward, mirroring `init`'s own guarantee. Both write at mode `0600`, matching `config.Patch`'s
  policy -- an operator who runs `init` and later hand-edits a real `apiKey` into the file should
  never find it world-readable.
- **The status page binds loopback-only by construction, not by convention.**
  `tray.normalizeLoopback` rewrites a bare `":port"` (which `net/http` would otherwise bind to
  every interface) to `"127.0.0.1:port"` before `ListenAndServe` ever runs -- CodeQL is a required
  check and a wide-open bind on a page that renders local filesystem paths (and, after M2, queue
  depth) is exactly the kind of thing it flags. The page also never renders `server.apiKey`;
  `TestHandleIndexRendersStatus` pins both (loopback rewrite + no-secret-leak) as regression
  tests.
- **Queue status is a real `internal/queue.Store.Counts` readout (issue #32), never a fabricated
  number when it can't be.** `tray.QueueStatusStub` is gone; `tray.QueueStatus.Configured=false` is
  the "not configured" signal (no `offline.queueDbPath` set, so the tray never opened a
  `QueueReader` at all) and `QueueStatus.Err` is a *distinct* signal (queue.db opened but a
  `Counts()` call itself failed) -- neither case renders as `0 pending`.
  `TestHandleIndexNeverFabricatesQueueNumbers` is the regression test, replacing the old
  `TestStatusQueueStatusIsAlwaysTheStub`. Same "no fake numbers" discipline the M1 gate's step 2b
  documents for `node_metadata` counts.
- **The tray's drain and prune timers use deliberately different locking, and both skip a busy
  tick rather than queue behind it.** `Runner.TriggerDrain` (issue #32) uses its own dedicated
  `drainMu`, never `Runner.gate`: `gate` is held for an ingest's or a self-update apply's entire
  duration, and a 5s drain timer sharing it would drop every tick during the exact window the
  queue is filling, while a drain pass holding `gate` across a slow NAS copy would block an
  inserted card for minutes. `Runner.TriggerPrune` DOES share `gate` (via `TryLockIdle`), because
  prune deletes from `ingest.localEditRoot` while an ingest can be writing into it -- a hazard that
  only exists once prune and ingest share one process, which standalone `prune -watch` never had
  to guard against. Both are `TryLock`-style: a tick arriving while a previous pass is still
  running skips outright (Drain/Pass are stateless across calls; `queue.db`'s own next-attempt
  columns are the real backoff), so passes can never pile up. See
  `docs/offline-queue.md`'s "Single writer once the tray is running" section for the
  `queue-drain -watch`-alongside-a-running-tray note this change makes newly relevant.
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
- **Login-item registration is off by default; self-update *checking* is on by default, but
  self-update *applying* is never automatic regardless of that flag.** `tray.startOnLogin:
  false` stays off-by-default (it registers this binary with the OS's own login-item mechanism,
  a system-integration change an operator should opt into). `selfUpdate.enabled` defaults to
  `true` in both `internal/config.defaultConfig()` and `config.example.yaml` -- deliberately
  different from every other opt-in flag in this repo, because a Check is a read-only GitHub API
  call, never a download or a write, and an operator who never learns a release exists can't act
  on it. `cmd/branchdam-agent/tray.go` reads `selfUpdate.enabled` directly off `config.Config`.
  `branchdam-agent update`'s explicit invocation is itself an operator's consent to *apply* an
  update, but it is not a separate switch that bypasses the config flag -- `runUpdateCmd` refuses
  outright when `selfUpdate.enabled` is false, so there is exactly one thing controlling whether
  the binary is willing to reach GitHub at all, checked the same way from both the tray's
  background check and this subcommand. Any config fixture used in a test that reaches
  `newSelfUpdateAgent`/`runUpdateCmd` and does not want a real network call must set
  `selfUpdate.enabled: false` explicitly -- see `cmd/branchdam-agent/tray_test.go`'s
  `TestRunTrayUnsupportedOnLinux` fixture for why (its self-update check goroutine is
  fire-and-forget, joined via `ctx` cancellation, not a `WaitGroup`).
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
- **Nothing is written to disk that didn't match a published `SHA256SUMS.txt` hash.**
  `internal/selfupdate.NewUpdater` is the only constructor and it always sets a
  `su.ChecksumValidator`; the package-level `su.DetectLatest`/`su.UpdateCommand` helpers (which
  use a validator-less default updater) are never called. This is the *only* integrity control
  in the whole release pipeline -- nothing published by `release-binaries.yml` is code-signed or
  notarized.
- **Self-update never applies while an ingest is in flight, and the guarantee is held for the
  whole download-and-apply window, not sampled once before it starts.**
  `Runner.TryLockIdle()` acquires the same `gate` `TriggerIngest` holds for its own whole
  duration; the caller keeps the returned `release` func for as long as the apply takes. A
  point-in-time `Runner.Busy()` check before a multi-minute download would still let the
  card-detection goroutine start an ingest that the binary swap then writes underneath.
- **`internal/selfupdate.Apply` never calls go-selfupdate's own `UpdateCommand` helper.**
  `UpdateCommand` compares versions with `Equal`, not `GreaterThan` -- unguarded, a re-tagged or
  yanked release could downgrade a workstation -- and it only ever targets one path, but a
  Windows install has two executables that must never drift to different versions (see below).
  `Apply` re-asserts `GreaterThan` itself and applies through `InstallLayout`'s ordered targets
  instead.
- **On Windows, one `Apply` call replaces both `.exe`s, sibling first, running exe last.**
  `internal/selfupdate.DetectLayout` finds the sibling by a hardcoded name pair
  (`branchdam-agent.exe`/`branchdam-agent-tray.exe`), never by munging the running exe's
  basename -- go-selfupdate's `DecompressCommand` fails outright for a name that isn't actually
  in the archive, which would abort the whole apply for a renamed exe. The sibling is applied
  first because go-selfupdate's cleanup of the old binary succeeds immediately there (nothing
  has it open); applying the running exe last means a failure on the sibling aborts before the
  running binary is touched, rather than leaving the two at different versions.
- **Self-update requires a per-user install location.** Applying needs write permission on the
  target's containing directory (a temp file is created and renamed over it); a
  `/Applications` or `C:\Program Files\` install cannot update itself without elevation.
  `internal/selfupdate.checkWritable` probes every target directory *before* downloading
  anything and returns `ErrTargetNotWritable` -- a system-wide install is update-by-reinstall,
  not a code-side failure mode to silently swallow.
- **The tray relaunches after a successful self-update via spawn-then-exit, from
  `cmd/branchdam-agent/tray.go`'s `runTrayCmd` strictly after `wg.Wait()` confirms the status
  server's listener is released -- never `syscall.Exec`, and never from inside `tray.Run`'s
  select loop.** `syscall.Exec` doesn't exist on Windows at all, and the running `.exe` has
  already been renamed aside to a hidden `.old` by the time `Apply` succeeds there, so Windows
  needs the spawn path regardless -- using `syscall.Exec` on darwin/linux would only add a
  platform split for no shared benefit. Relaunching before the listener closes would make the
  successor's own bind fail, since `StatusServer.Listen()` (called before `tray.Run` starts) is
  this tray's single-instance guard, not a separate lock file.
- **macOS self-update replaces only the bundle's inner binary, then rewrites `Info.plist`
  locally -- it never re-downloads or replaces the bundle itself.** go-selfupdate's
  `unarchiveTar` matches an archive entry by base name regardless of its path inside the
  archive, so it only ever extracts and swaps
  `branchdam-agent.app/Contents/MacOS/branchdam-agent`. `internal/selfupdate.Apply` writes a
  freshly rendered `Info.plist` immediately after, via the same `internal/appbundle.RenderInfoPlist`
  `tools/mkbundle` uses at build time, so the two are structurally unable to produce different
  plists.
- **The `.app` bundle is never code-signed, even opportunistically.** Go's linker already
  ad-hoc-signs the inner `darwin/arm64` binary at build time, which is what keeps a binary-only
  in-place replacement valid; signing the *bundle* on top would bind that signature to bundle
  contents self-update then mutates, and macOS would refuse to launch the result as "damaged."
- **Self-update refuses to run from a Gatekeeper-translocated path.**
  `internal/selfupdate.DetectLayout` returns `ErrTranslocated` for any resolved path containing
  an `AppTranslocation` path segment -- such a mount is read-only and disappears on reboot, so
  writing a replacement binary there, or baking the path into a login item, would silently fail
  or produce a dangling reference. The fix is operator-facing (install to `/Applications` or
  `~/Applications`), not something the code can route around.
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
`binaries` job -- see below and the recursion-guard invariant above), and `hermes.yml`
(automated PR review by the `s3ntin3l8-hermes[bot]` GitHub App, via the shared
`s3ntin3l8/.github/.github/workflows/hermes-review.yml@main` reusable workflow). The
`hermes-review.yml` reusable workflow requires `runs-on: self-hosted` -- unlike every other
workflow in this repo, it does not run on a GitHub-hosted runner, because the runner only relays
the request to a Hermes API server reachable over the LAN (hermes-01), which is where the agent
actually executes and posts the review back to GitHub itself. This mirrors branchDAM's own
`hermes.yml` (s3ntin3l8/branchdam#76) byte-for-byte except for the header comment's repo-specific
runner name; both jobs (`auto-review` on `opened`/`ready_for_review`, `on-demand-review` on an
`@s3ntin3l8-hermes` mention via `issue_comment`) pin `mention-trigger: s3ntin3l8-hermes`, since
the reusable workflow's own default (`hermes`) won't match the App's actual slug. Requires,
per-repo, a self-hosted runner registered against *this* repo (self-hosted runner registrations
are repo-scoped on a personal GitHub account -- there is no org-wide pool to fall back on), the
`s3ntin3l8-hermes` GitHub App installed on this repo, and the `HERMES_APP_ID`/
`HERMES_APP_PRIVATE_KEY`/`HERMES_API_KEY` repo secrets set (`gh secret set <name> --repo
s3ntin3l8/branchdam-agent`) -- none of which this file can assert are actually done; check the
repo's Settings -> Actions -> Runners and Settings -> Secrets before relying on this workflow
firing.

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
- **Addressing review feedback (Hermes or human).** Fixing the code alone is not enough --
  always reply to and resolve the inline conversation too, same convention as branchDAM's own
  CLAUDE.md. Ask for another Hermes pass at any point, including after addressing feedback, by
  commenting `@s3ntin3l8-hermes Review` on the PR (or `@s3ntin3l8-hermes Triage` on an issue).
  Auto-review only fires once per PR, on `opened`/`ready_for_review` and only for PRs from this
  repo, not forks -- see `hermes.yml`'s own comments for why.

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
