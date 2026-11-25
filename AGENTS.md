# AGENTS.md — branchdam-agent

This file holds this repo's load-bearing invariants and repo-specific facts: the ones every agent needs before touching anything, whichever CLI it runs in. `CLAUDE.md` is a one-line `@AGENTS.md` import, so every CLI (Claude Code, Codex, opencode, agy) reads this file, natively or through that import. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for branch protection and release details, and [`docs/`](docs) for deeper architecture detail.

Workstation companion agent for branchDAM. Handles SD card ingestion, dual-copy
verified storage writes, offline queueing, and catalog sync. Go binary
(cross-platform: Linux, Windows, macOS) with system tray UI.

<!-- mullion:briefing:start -->
This repo's correctness invariants are summarised as a checklist in `.claude/skills/branchdam-agent/SKILL.md`. It covers changes that look harmless but break another package's assumptions: card re-reads, verify cache bypass, queue-before-network ordering, prune hash equality, tray `gate`/pause semantics, config patching and key rotation. Read it before changing `internal/ingest`, `internal/queue`, `internal/prune`, `internal/tray`, `internal/config`, `internal/branchdam` or `internal/naming`. For the self-review pass, or when delegating review of uncommitted changes, use the `branchdam-agent-reviewer` subagent (`.claude/agents/branchdam-agent-reviewer.md`), which applies that checklist to the diff.

## Workflow Conventions

- Never commit directly to the default branch. Always branch and open a PR.
- Branch off the latest remote default branch, never off your local one: `git fetch origin && git checkout -b <branch> origin/<default>`. A local default branch is routinely stale, which is what makes a PR show up as "out-of-date with the base branch" the moment it's opened.
- Work in a dedicated worktree per branch, under `.wt/<name>` (the branch name with `/` replaced by `-`, so the directory stays flat), off the latest remote default branch: `git fetch origin && git worktree add .wt/<name> -b <branch> origin/<default>`. A fresh worktree does not inherit installed dependencies — reinstall them there before testing or building.
- Commit and PR titles need a Conventional Commits prefix (`feat:`, `fix:`, `chore:`, ...).
- Squash-merge PRs — the PR title becomes the commit message on the default branch.
- Require green CI before merging.
- Run a self-review pass on your own diff before declaring done.

  Request an automated/bot review through the review relay (`mullion review`, or the `request_review` tool) — once after opening the pull request, and again per head commit after pushing fixes for a changes-requested review, then wait for it. A review comment that mentions the review bot is for humans, not agents; if no relay is set up, tell the human instead. One review per pull request at a time: a review that is already running is not re-requested — the request is refused with its run id, and that refusal is the answer, not an error to retry: wait for it, or stop it with stop_review. A review takes 10-30 minutes; check review_status once instead of asking again. After pushing fixes for a changes-requested review, ask again only when no review is in flight. Once the review bot has approved, apply its remaining suggestions and push without requesting another review, even though the head moved: small targeted fixes do not change the verdict, and re-reviewing them only breeds more nits. Never re-trigger a review with an @mention of the review bot: that path is for humans, and it queues another round behind the running one.
- Fixing the code is not enough to address review feedback — reply to each inline comment via the GitHub API, then resolve the thread via the GraphQL `resolveReviewThread` mutation, rather than pushing a silent fix. Reply with `gh api repos/{owner}/{repo}/pulls/<pr>/comments/<comment_id>/replies -f body="…"`; resolve with `gh api graphql -f query='mutation { resolveReviewThread(input: {threadId: "<thread_id>"}) { thread { isResolved } } }'`, taking the thread id from a `reviewThreads` query on the pull request, not from the REST comment id.
- File a tracked issue for anything a plan defers, blocks, or descopes, and link it from the PR (using the repository's issue template if it has one) — a footnote in a plan doc is not a durable record. Write each one as a spec an agent can implement on its own (scope, acceptance criteria), not as a note. Title it the way this repo titles commits and PRs, reusing its existing scope names rather than coining new ones; attach it as a sub-issue to the matching open epic when one exists; and set GitHub's `blocked_by` dependency where one issue cannot start before another closes.
- After a merge, delete the local and remote branch and remove any associated worktree.
- Before pushing, run the full lint/typecheck/test/format gate.
<!-- mullion:briefing:end -->

## Repo specifics

- The pre-push gate is two targets, and neither covers the other:
  - `make lint` runs pre-commit: gofmt, go vet, `go mod tidy -diff`, `go test -race`, golangci-lint and detect-secrets.
  - `make check` runs build, vet, `go test -race` with coverage, `make vulncheck`, `build-windows` and `build-darwin`. It does not need pre-commit.
  - `make install-hooks` also installs a pre-push hook that runs `make vulncheck`.
- `make vulncheck` needs `jq`. Its allowlist is `VULNCHECK_IGNORE` in the Makefile (default `GO-2026-5932`), mirrored by `govulncheck-ignore` in `ci-cd.yml`. Change both together.
- golangci-lint is built with `go install` into a repo-local `.bin/`, using a Go toolchain floor of 1.26.7. Older Go panics in its buildir pass (issue #163).
- `make build-darwin` is a build-only check that excludes `internal/tray` and `cmd/branchdam-agent`, because the darwin tray needs cgo. `make build-darwin-app` and `build-darwin-dmg` run only on a macOS host.
- Release Please builds the changelog from the squash-merged PR title, so an unprefixed title drops out of the changelog. Releases go out as pre-releases (`"prerelease": true`). Any change to `internal/queue/store.go` or `internal/queue/migrations.go` relies on this; read CONTRIBUTING.md's "Pre-release gating" before touching the queue schema.
- The review bot is Hermes (`s3ntin3l8-hermes[bot]`). It auto-reviews every non-draft same-repo PR on open; request reviews as the Workflow Conventions describe. `required_conversation_resolution` is on: every inline thread blocks merge until replied to and resolved.
- Release-please's own PR never gets CI checks reported. That is expected; see CONTRIBUTING.md.
- Branch DTOs are conformance-tested against branchDAM fixtures: refresh them with `make contract-sync BRANCHDAM=<branchdam checkout>`, never by hand-editing `internal/branchdam/testdata/contract/`. The hashing golden vectors were transcribed by hand from a pinned branchDAM commit and are not touched by `contract-sync`.
- **Skill and reviewer exist twice:** `.claude/skills/<n>/SKILL.md` ↔ `.agents/skills/<n>/SKILL.md` and `.claude/agents/<n>.md` ↔ `.agents/skills/<n>/SKILL.md` (same body; Claude Code reads `.claude/`, the other CLIs read `.agents/`). Edit both together; `scripts/check-agent-mirrors.sh` (pre-commit) fails on drift.

## Commands

```sh
# Pre-PR Gates & Testing
make check           # build + vet + test -race (coverage) + vulncheck + windows/darwin builds
make lint            # pre-commit: gofmt, vet, tidy, test -race, golangci-lint, detect-secrets (run both)

# Cross-compilation Builds
make build-windows   # Builds dist/branchdam-agent.exe and dist/branchdam-agent-tray.exe
make build-darwin    # Build-only check for darwin/arm64
make build-darwin-app# Assembles macOS .app bundle via tools/mkbundle
```

## Package Responsibilities

| Package | Responsibility |
|---|---|
| `cmd/branchdam-agent` | Subcommand dispatch (`preflight`, `ingest`, `tray`, `update`, `prune`, `luminar-sync`, `init`, hidden `dialog`) |
| `internal/branchdam` | REST client for branchDAM `/api/v1/agent/*` contract (`hello`, `handshake`, `events`, `rebase`, `node-status`, `check-content`, `upload` (on its own HTTP client with no total timeout), `telemetry` (posted by the tray's own 15-minute timer via `Runner.TriggerTelemetry`, skipped while paused or config-incomplete)) |
| `internal/ingest` | DualWrite (streaming multi-writer), cache-defeating Verify, EXIF/SRT metadata extraction, naming engine |
| `internal/queue` | Local SQLite queue (`queue.db`) persisting ingest records and retry states for offline field operation |
| `internal/tray` | Menu bar / system tray companion (Windows/macOS) with loopback HTTP status server (`127.0.0.1:38080`, `config.DefaultStatusAddr`; rejects any `Host` that isn't a loopback name/IP on its port, against DNS rebinding) |
| `internal/selfupdate` | Checksum-validated self-update (`go-selfupdate`) with multi-target apply that is all-or-nothing for any failure the process survives (a failed target restores the ones already swapped; the rollback sidecar is written only after every target swaps; a crash mid-apply is not covered -- see `docs/platform-support.md`) and instant rollback |
| `internal/luminar` | Read-only Luminar Neo catalog parser (schema verified against `db_version 155`) generating Tier-2 lineage edges from filename-inferred pairing -- see `docs/luminar-catalog.md` |
| `internal/resolvehook` | Detects and installs DaVinci Resolve's render hook into its `Scripts/Utility` folder (`CandidateDirs`/`Detect`/`Install`, atomic temp-then-rename write) |
| `internal/nodeindex` | File path to `nodeUuid` lookup resolver |
| `internal/hashing` | Ported xxHash64 `FastHash`, `StreamingFastHasher`, BLAKE3 `FullHash`, and `PerceptualHash` |
| `internal/config` | YAML config loader with environment expansion and surgical `yaml.Node` atomic patcher |
| `internal/exiftool` | Pooled `exiftool -stay_open` subprocess manager (`Pool`), shared by `internal/ingest`'s EXIF extraction and `internal/phash`'s RAW preview fallback |

## Key Invariants

One line each; the full statement (with edge cases, issue history and file pointers) is in
[`docs/invariants.md`](docs/invariants.md) under the same number. Read the full entry before changing the code it
names.

1. **Streaming Dual-Write**: the card is read once (`DualWrite` to local SSD + NAS, or one verified temp copy in upload mode), hashing xxHash64 + BLAKE3 in flight. Never re-read the card to hash.
2. **Cache-Defeating Verify**: fsync, close, re-open unbuffered; fall back to buffered only at open time and only on the "unbuffered unsupported" errors. macOS `F_NOCACHE` is advisory.
3. **Offline Queue Safety**: ingest intent is in `queue.db` before any network call (offline always, online when `queueDbPath` is set, metered-deferred uploads as `UPLOAD` rows); a POST that isn't accepted stays `Queued` for `Drain`. Queue schema changes ship as pre-releases (CONTRIBUTING.md).
4. **Path Translation**: payload paths are container-absolute via `pathMappings` (longest prefix) before dispatch.
5. **Double-Encoded Event Envelopes**: `/agent/events` payload is a JSON string; `evidenceJson` inside `EVENT_EDGE_ATTACHED` is a raw object.
6. **Surgical Config Patching**: `patch.go` edits the `yaml.Node` AST, keeps comments and `${VAR}` placeholders, writes atomically (0600, fsyncs), serialises on `patchMu`, and replaces a symlink's target, never the link.
7. **Client-Side Edge Validation**: `ValidateEdgeAttached` enforces confidence `[0.50, 1.00]` and allowed types, and rejects a client `reviewState`.
8. **Prune Safety**: only `origin = OFFLINE` rows with archive `DONE`, and only when `node-status` reports a live, `TIER3_MASTER_ARCHIVE`, `verified` node **and** its `fullHash` equals the BLAKE3 recorded at ingest; a missing or mismatched hash keeps the file. Re-verifies containment and mtime/size before deleting.
9. **Tray Gate Serialization**: `TriggerIngest` serialises on `gate`; `Reconfigure` never waits on it and applies the newest pending config once it frees, never swapping the engine mid-copy. Window actions are thin drivers over `Runner` (`TryLock`, never blocking); `CancelIngest` stops only at a file boundary and never auto-ejects.
10. **`naming.Stem` Is a Locked Port** of branchDAM's; never add Luminar suffixes (Luminar has its own local `stem`).
11. **LastHandshakeAt Carry-Forward**: a failed drain must not erase the last successful stamp; the zero value hides the row.
12. **Runtime State Persistence**: `LastHandshakeAt` persists to `internal/runtime`'s state file (0600/0700, atomic + dir fsync), re-seeded with `HandshakeOK=false`; write failures never block a drain.
13. **Ingest Core Has No UI Imports**: `internal/ingest` stays UI-free; `internal/tray` drives it, never reimplements it.
14. **Detector Lifecycle**: `ReconfigureDetector` cancels and restarts `Detector.Watch` on root changes; empty roots stop it (except Windows removable-drive polling). Settings reloads restart it only via `Reconfigure` after the gate frees.
15. **Tray Pause Gates**: session-only `paused` short-circuits ingest/drain/prune and holds detector insertions without cancelling an in-flight ingest; resume re-offers held volumes through the normal gate. The persisted `pauseUploadOnMetered` gate holds drains and uploads on a metered link and fails closed at the call site (a probe error counts as metered).
16. **Content Dedup Fails Open**: `checkContentDedup` runs after the copy, before `Verify`, on in-flight hashes (pre-screen, then `fullHash`); a live match removes only the agent's own just-written copies; every failure falls open, with a per-pass `dedupState` latch. Upload mode relies on the server's `DEDUPLICATED` instead.
17. **M5 Tray UX Wiring**: `IngestGate` confirms detection ingests only (manual bypasses it); `RequireDCIM`/`AllowedExtensions` filter volumes/files; `PathTemplate` is overwritten by each handshake; `autoEject` only after an `OK()` detection ingest of a card volume root.
18. **Rotated API Keys**: store the rotated key durably, then switch, then confirm with a handshake; never overwrite a `${VAR}` placeholder; a `401` on first use reverts once and never re-adopts that key id.
