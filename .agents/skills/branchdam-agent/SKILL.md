---
name: branchdam-agent
description: Correctness invariants for branchdam-agent (SD-card ingest, dual-write/verify, offline queue.db, prune, tray Runner gates, config patching, API-key rotation). Read this before changing or reviewing anything under internal/ingest, internal/queue, internal/prune, internal/tray, internal/config, internal/branchdam or internal/naming. These are changes that look harmless on their own but break something another package depends on.
---
<!-- mullion:scaffold:branchdam-agent -->

# branchdam-agent invariants

AGENTS.md "Key Invariants" is the full statement of each rule. This file lists the mistakes that most often break them. Each entry gives the edit that looks reasonable, why it is wrong, and where the rule lives.

## Ingest data path (`internal/ingest`)

- **Never read the card a second time.** `DualWrite` (`writer.go`) streams the card once to both destinations and computes xxHash64 + BLAKE3 while copying. Do not hash the card again, re-open the source in `Verify`, or `checkContentDedup`. Upload mode is the same: the card goes once into a verified local temp copy, and the upload streams that copy with `X-Blake3-Hash`. If "just hash the source first" appears anywhere, it is a bug.
- **Verify must bypass the page cache.** `Verify` (`verify.go`) does fsync, close, then reopens unbuffered (`O_DIRECT` / `F_NOCACHE` / `FILE_FLAG_NO_BUFFERING`).
  - The buffered fallback is chosen once, when the file is opened. It never happens mid-stream.
  - The fallback is allowed only for the "unbuffered unsupported" errors: `EINVAL`/`EOPNOTSUPP` on Linux, `ENOTSUP` on macOS, and `windowsUnbufferedUnsupported` (`verify_fallback.go`) on Windows. Widening that error set, or falling back on any other open error, silently turns verify into a cache read.
  - Never bypass `requireUnbuffered`, including the "identical copies already exist" re-run path.
- **Dedup runs after the copy and before Verify, and it fails open.**
  - A confirmed live match (`ACTIVE`/`HIDDEN`) online deletes only the two O_EXCL copies `DualWrite` itself just created. Offline, it returns before the queue row is written.
  - A pre-existing copy is never deleted.
  - A match at this file's own archive container path is not a duplicate.
  - Any dedup failure must fall open (ingest continues).
  - In `dedupState`, a 400 on the pre-screen sets `fullOnly`. It must never latch `unavailable`. Only transport errors, 404/501, 401/403 and 429 latch `unavailable`.
- **Re-runs never skip silently.** Identical copies at both destinations are skipped only when there is evidence the file was registered: a queue row claims it, or `check-content` reports a live node at its own container path. Without that evidence, the file is registered.
- **Translate the path before writing.** Map through `ToContainerPath` (longest-prefix `pathMappings`) before `DualWrite` and before any payload is sent. A workstation path in a payload is a bug.
- **Cancellation:** check `ctx` per file and per read buffer. `CancelIngest` uses `WithFileBoundaryCancel`: the file in flight still finishes copy, verify and registration/queue row. The summary must report `Canceled` and never `OK()`, so the card does not auto-eject.
- `internal/ingest` must not import UI or windowing packages (no `fyne.io/systray`).

## Offline queue (`internal/queue`, `ingest/drain.go`, `offline*.go`)

- **Write intent to the queue before any network call.**
  - Offline ingest always writes a row first.
  - Online ingest with `offline.queueDbPath` set writes an `origin = ONLINE` row before the `EVENT_NODE_CREATED` POST. A POST that is not accepted leaves the row for `Drain`, and the file reports `Queued`, not failed.
  - A metered-deferred upload writes an `origin = UPLOAD` row before any network call and reports `Queued`, not `Deferred`. Without a queue, or if the insert fails, it stays `Deferred`.
  - Reordering an insert to after the POST loses files when the network fails mid-ingest.
- **Migrations:** forward-only and idempotent. Each step runs in one transaction together with its `PRAGMA user_version` bump. Pragmas go in the DSN `_pragma` list, so every pooled connection gets them; a one-off `db.Exec("PRAGMA …")` covers only one connection. The path is escaped as a `file:` URI.
  - Any schema change (`store.go`, `migrations.go`) relies on releases going out as pre-releases (see CONTRIBUTING.md).
- **GC and terminal states:**
  - A row with a `PENDING` upload is never terminal.
  - GC deletes only rows that finished successfully, are older than 90 days, and whose local copy is gone.
  - Never collect `FAILED`/pending rows, or rows `prune` still needs.
- **`CopyToArchive`** copies the local file's mtime to the archive copy and fsyncs the directory after the rename.
  - An archive file of exactly the local copy's size that reads back wrong is a read-back mismatch. It is not a conflict and is never removed.
  - Two consecutive mismatches set `archive_copy FAILED`.
- `Drain` waits `MinRebaseDwell` (4s) before `/rebase`. Do not shorten it or "optimise" it away.
- A queued upload re-stats the local copy against the recorded size and mtime before sending, and fails the row if the copy changed.

## Prune (`internal/prune/prune.go` `Pass`)

- Prune candidates are allowlisted to `origin = OFFLINE` with archive copy `DONE`. Never broaden this to a denylist; `ONLINE` and `UPLOAD` rows must stay excluded.
- A file is deleted only when node-status reports found, live (`ACTIVE`/`HIDDEN`), `TIER3_MASTER_ARCHIVE`, `verified`, **and** the server's `fullHash` equals the BLAKE3 hash `queue.db` recorded (case-insensitive compare). `verified` alone is not enough.
- A missing server hash, a mismatch, or no local hash means **keep the file**, counted as `no-server-hash` / `hash-mismatch` / `no-local-hash`. Each gets a `[SKIP]` line plus a pass-level `slog.Warn`, because the tray throws away per-file output.
- Re-check local containment and on-disk mtime/size immediately before deleting.

## Tray Runner concurrency (`internal/tray`)

- **`gate`:** `TriggerIngest` serialises ingests behind `gate`.
  - Window/API-driven ingest uses `TriggerIngestNoWait` (`TryLock` → `ErrGateBusy`, plus a notification naming the volume). Adding a blocking `Lock` there leaves a batch stuck where `CancelIngest` cannot reach it.
  - `Reconfigure` never waits on `gate`. It records the newest call and applies it when the gate frees. Never swap the engine under a card that is mid-copy, and `Close` the engine it replaces (that shuts down its exiftool pool).
  - A settings reload never restarts the detector directly, because that would cancel a detection-triggered ingest running on the detector's context. Record the interval/requireDCIM change and let `Reconfigure` restart the detector after the gate frees.
  - Rollback uses `TryLockIdle` then `applyMu`, the same as `StartApply`.
- **Pause:**
  - `paused` is session-only. Never persist it.
  - Pausing holds detector insertions (`pausedInserts`). It never drops them and never cancels an in-flight ingest.
  - `pauseUploadOnMetered` is persisted config.
- **Metered-link gate:** `TriggerDrain`'s gate is `metered || mErr != nil`, using a synchronous probe; it fails closed.
  - The menu loop must never call the probe. It reads the cached value in `metered.go`.
  - `UploadGate` returns `metered = true` along with the probe error, and also holds while paused.
- **Handshake timestamp:** a failed drain carries forward the prior non-zero `LastHandshakeAt`. The startup seed from the runtime state file sets `HandshakeOK=false`. A state-file write failure is only `slog.Warn` and must never block a drain.
- **Auto-eject** happens only for a detection-triggered ingest of exactly a card volume root, and only when the summary is `OK()`. Manual paths never eject. `OK()` is false while any upload is `Deferred`.
- Failed rows are re-queued with `ResetFailedNode`/`ResetFailed`. This takes no lock, because a drain never selects a FAILED row.
- The loopback status server rejects any non-loopback `Host` (protection against DNS rebinding). Do not relax that check.

## Config and keys (`internal/config`, `internal/branchdam`)

- **Config writes** go through `Patch` (`patch.go`), which edits the `yaml.Node` AST.
  - Never marshal a struct and overwrite the file: that strips comments and expands `${VAR}` placeholders.
  - Writes are atomic: 0600, temp file fsynced, rename, then directory fsync.
  - Writes are serialised by `patchMu`.
  - A symlinked `config.yaml` has its target replaced, never the link itself.
- **Key rotation** (`keyrotation.go`):
  - Open `sealedApiKey` with the key that authenticated the handshake. Never use a cleartext `apiKey`.
  - Persist the new key durably via `SetRotatedAPIKey` *before* swapping the key in memory.
  - Never overwrite a `${VAR}` placeholder. Return `ErrAPIKeyPlaceholder` and never echo the placeholder text.
  - A 401 on the new key's first use reverts to the old key, retries once, and never re-adopts that key id.
  - Each request reads `currentKey` once and signs with that same value.
- **Events:** the `/events` payload is a double-encoded JSON string, but `evidenceJson` inside `EVENT_EDGE_ATTACHED` is a raw JSON object. `ValidateEdgeAttached` requires confidence in [0.50, 1.00] and an allowed relationship type, and rejects any agent-supplied `reviewState`.

## Locked ports

- `internal/naming.Stem` is a byte-for-byte port of branchDAM's own function. Never add suffixes to it, Luminar-specific or otherwise. Luminar pairing uses its own local `stem` in `internal/luminar/derive.go`.
- `internal/hashing` and the branchDAM DTOs are conformance-tested against fixtures from branchDAM. Refresh them with `make contract-sync BRANCHDAM=<checkout>`. Do not edit golden data by hand to make a test pass.

## Cross-platform

- Platform-tagged files (`verify_*`, `internal/eject`, `internal/netgate`, `internal/tray` windows/darwin split) need `make build-windows` / `make build-darwin`. `build-darwin` excludes `internal/tray` and `cmd/branchdam-agent`, because the darwin tray needs cgo.
- macOS `F_NOCACHE` is advisory. Do not document or test a macOS unbuffered verify as a guaranteed cache bypass.
