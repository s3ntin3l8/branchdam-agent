# AGENTS.md — branchdam-agent

This file is the single source of truth for this repo's workflow rules and
load-bearing invariants — the ones every agent needs before touching
anything, regardless of which CLI you are. `CLAUDE.md` is a one-line
`@AGENTS.md` import, so every CLI (Claude Code, Codex, opencode, agy) reads
this file, natively or via that import. See [`CONTRIBUTING.md`](CONTRIBUTING.md)
for the contributor workflow and [`docs/`](docs) for deeper architecture detail.

Workstation companion agent for branchDAM. Handles SD card ingestion, dual-copy
verified storage writes, offline queueing, and catalog sync. Go binary
(cross-platform: Linux, Windows, macOS) with system tray UI.

## Commands

```sh
# Pre-PR Gates & Testing
make check           # build + vet + test (race detector + coverage)
make lint            # pre-commit run --all-files (golangci-lint + vet + detect-secrets)

# Cross-compilation Builds
make build-windows   # Builds dist/branchdam-agent.exe and dist/branchdam-agent-tray.exe
make build-darwin    # Build-only check for darwin/arm64
make build-darwin-app# Assembles macOS .app bundle via tools/mkbundle
```

## Package Responsibilities

| Package | Responsibility |
|---|---|
| `cmd/branchdam-agent` | Subcommand dispatch (`preflight`, `ingest`, `tray`, `update`, `prune`, `luminar-sync`, `init`, hidden `dialog`) |
| `internal/branchdam` | REST client for branchDAM `/api/v1/agent/*` contract (`hello`, `handshake`, `events`, `rebase`, `node-status`, `check-content`) |
| `internal/ingest` | DualWrite (streaming multi-writer), cache-defeating Verify, EXIF/SRT metadata extraction, naming engine |
| `internal/queue` | Local SQLite queue (`queue.db`) persisting ingest records and retry states for offline field operation |
| `internal/tray` | Menu bar / system tray companion (Windows/macOS) with loopback HTTP status server (`127.0.0.1:38080`, `config.DefaultStatusAddr`) |
| `internal/selfupdate` | Checksum-validated self-update (`go-selfupdate`) with atomic apply and instant rollback |
| `internal/luminar` | Read-only Luminar Neo catalog parser (schema verified against `db_version 155`) generating Tier-2 lineage edges from filename-inferred pairing -- see `docs/luminar-catalog.md` |
| `internal/resolvehook` | Detects and installs DaVinci Resolve's render hook into its `Scripts/Utility` folder (`CandidateDirs`/`Detect`/`Install`, atomic temp-then-rename write) |
| `internal/nodeindex` | File path to `nodeUuid` lookup resolver |
| `internal/hashing` | Ported xxHash64 `FastHash`, `StreamingFastHasher`, BLAKE3 `FullHash`, and `PerceptualHash` |
| `internal/config` | YAML config loader with environment expansion and surgical `yaml.Node` atomic patcher |
| `internal/exiftool` | Pooled `exiftool -stay_open` subprocess manager (`Pool`), shared by `internal/ingest`'s EXIF extraction and `internal/phash`'s RAW preview fallback |

## Key Invariants

1. **Streaming Dual-Write (`DualWrite`)**: Ingestion reads the camera card once, streaming bytes to both the local edit SSD and NAS archive simultaneously while computing xxHash64 and BLAKE3-256 in-flight. Never re-reads the card to compute hashes.
2. **Cache-Defeating Verification (`Verify`)**: Flushes (`fsync`), closes, and re-opens files unbuffered (`O_DIRECT` on Linux, `F_NOCACHE` on macOS, `FILE_FLAG_NO_BUFFERING` on Windows). If unbuffered open fails (e.g. tmpfs or CIFS), falls back to the buffered floor once at open time; never mid-stream.
3. **Offline Queue Safety (`queue.db`)**: Ingest intent is persisted in local SQLite before network calls. Offline drain enforces `MinRebaseDwell` (4s) to guarantee server-side event consumption before emitting `/rebase`.
4. **Path Translation (`ToContainerPath`)**: Agent payload paths are container-absolute, translated client-side via `pathMappings` (longest-prefix match) before dispatch.
5. **Double-Encoded Event Envelopes**: `/api/v1/agent/events` payload is a double-encoded JSON string; inner `evidenceJson` in `EVENT_EDGE_ATTACHED` is a raw JSON object.
6. **Surgical Config Patching (`patch.go`)**: Config writes mutate raw `yaml.Node` ASTs and write atomically (0600), preserving comments and unexpanded `${VAR}` placeholders.
7. **Client-Side Edge Validation**: `ValidateEdgeAttached` strictly enforces confidence `[0.50, 1.00]`, allowed relationship types, and rejects agent-supplied `reviewState` before POSTing.
8. **Prune Safety Checks**: `prune` only operates on `queue.db` items confirmed verified by `POST /api/v1/agent/node-status`. Re-verifies local containment and disk mtime/size before deletion.
9. **Tray Gate Serialization**: `Runner.TriggerIngest` serializes concurrent card insertions and manual clicks behind `gate`. Reconfiguration rebuilds components via `Runner.Reconfigure`.
10. **`internal/naming.Stem` Is a Locked Port**: byte-for-byte port of branchDAM's own `naming.Stem` (conformance verified against that repo's own golden test table). Its role-suffix pattern must never gain Luminar-specific suffixes — `internal/luminar/derive.go`'s own local `stem` helper exists so `PairDerivatives` doesn't need to touch it. See `docs/luminar-catalog.md`.
11. **LastHandshakeAt Carry-Forward (in-session)**: A failed drain pass must not erase the prior successful `LastHandshakeAt` -- the carry-forward in `TriggerDrain` keys off `r.lastDrain != nil && !r.lastDrain.LastHandshakeAt.IsZero()` (PR #148, Hermes review). Initial failures (no prior successful stamp) stay at the zero sentinel; the template's `{{ if not .Status.LastHandshakeAt.IsZero }}` guard suppresses the "last handshake" line in that case.
12. **Runtime State Persistence (`LastHandshakeAt`)**: `Runner.lastDrain.LastHandshakeAt` is persisted to a per-platform runtime state file (`internal/runtime`, sibling to `agent.log` in the XDG-state dir) on every successful drain pass and re-seeded at tray startup, so the status page's "last handshake: <since> ago" line survives a tray restart (issue #149 / audit F-13 cross-session half). The seed sets `HandshakeOK=false` -- the seeded stamp is from a *prior session's* successful handshake, and `Status().HandshakeOK` is the *current session's* "last drain: handshake OK" signal; a pre-seed `HandshakeOK=true` would briefly lie about the current session before the drain timer's first tick. Write failures are non-blocking (`slog.Warn`, callback return value is swallowed) and panicking callbacks are recovered -- a failing `os.WriteFile` must never block a drain pass. The runtime state file is distinct from `config.yaml` (operator-edited, never agent-written). File durability: the state file is created at 0o600 and its parent directory at 0o700, with a chmod-after-MkdirAll that retightens any pre-existing 0o755 dir; the atomic temp+rename write is followed by an fsync of the parent directory (POSIX `syscall.Fsync` on Linux/BSD, `fcntl(F_FULLFSYNC)` on macOS, no-op on Windows -- NTFS commits dir entries transactionally); chmod/fsync failures are logged at WARN and non-fatal; WriteFile/Rename failures return to the caller, which logs and continues so the drain pass never blocks on the freshness signal.
13. **Ingest Core Has No UI Imports**: `internal/ingest` is intentionally UI-free -- no `fyne.io/systray`, no platform-windowing deps -- so the same headless engine can be driven by `cmd/branchdam-agent/ingest` (M1's headless half) and `internal/tray` (M1's tray-resident shell), and so cross-compile properties for `internal/ingest` are unaffected by the tray's `windows/darwin` build-tag split. The tray package is a thin driver over the engine, not a reimplementation (see `internal/tray`'s package doc).
14. **Detector lifecycle (#78)**: `Runner.ReconfigureDetector` cancels the in-flight `Detector.Watch` via a stored `detectorCancel` + `detectorDone` (guarded by `detectorMu`) and starts a new goroutine when `cardRoots` changes; `Reconfigure(nil)` stops the detector. Replaces the "process restart" path. (`internal/tray/tray.go:1262-1305`)
15. **Tray pause gates (#83 + #84)**: Two independent session-only booleans (`paused` / `pauseUploadOnMetered`) guard the network side. `paused` (shoot-mode) short-circuits `TriggerIngest` / `TriggerDrain` / `TriggerPrune` and drops detector events, but never cancels an in-flight ingest. `pauseUploadOnMetered` + `netgate.IsMetered()` true/missing short-circuits `TriggerDrain` only — local edit copy proceeds regardless. A `netgate` probe error returns `(true, nil)` to fail closed. (`internal/tray/tray.go:241-257,766-829`; `internal/netgate/`)
16. **Pre-flight dedup with offline-aware fail-open (#88 + #82)**: `checkContentDedup` runs a 2-phase (fastHash → fullHash) lookup against `GET /api/v1/agent/check-content` before `WriteLocal` in both `IngestCard` (online) and `IngestCardOffline`. A 5s per-file timeout (`PreflightTimeoutSecs`) falls open. A latched `dedupUnavailable` flag skips remaining files in the same pass after a 404 / transport error; `IngestCardOffline` clears the latch per pass (offline may regain connectivity between cards). The offline auto-fallback in `TriggerIngest` (NAS unreachable → `IngestCardOffline` instead of error) is gated on `queueReader != nil`. (`internal/ingest/ingest.go:652`, `offline.go:98,261`)
17. **M5 tray UX wiring (#79 + #80 + #81 + #86 + #87)**: Five tray behaviors wired through Settings submenus and the headless `ingest` subcommand's `-allowed-extensions` override: (a) `IngestGate` confirmation dialog wrapping `TriggerIngest` with an `autoImportPaths` allow-list; a dialog-render error is NOT added to the skip set (only explicit refusals are). (b) "Import from folder…" manual source picker reusing the same `dialogRunner`. (c) `RequireDCIM` + `AllowedExtensions` filters applied at both detection (`ListVolumesUnder`) and walk time; empty `allowedExtensions` = "accept all" (backward-compat). (d) `PathTemplate` overwritten by `Handshake.NamingTemplate` at tray startup and on every `reload()` — operator edits survive only between Handshakes. (e) `autoEject` only fires when the ingest summary is `OK()`, via the platform-tagged `internal/eject` package (Linux: udev / `eject`; Darwin: `diskutil unmountDisk`; Windows: `FSCTL_LOCK_VOLUME` + `CM_Request_Device_Eject`; other: stub).

## Review thread resolution

Every review thread (Hermes or human) must be replied to and resolved before
a PR is mergeable. This is a GraphQL-only concept, not a `gh pr` verb:

```sh
# 1. Reply to inline comment (REST)
gh api repos/s3ntin3l8/branchdam-agent/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
# 2. Resolve thread (GraphQL)
gh api graphql -f query="mutation { resolveReviewThread(input: {threadId: \"<thread_id>\"}) { thread { isResolved } } }"
```
