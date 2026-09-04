# The offline queue (M2, issue #4)

`internal/queue` (`queue.db`, `modernc.org/sqlite`) plus `internal/ingest`'s `IngestCardOffline`
and `Drain` implement the offline-ingest case: a travelling workstation with no route to the NAS
that still hosts the Tier-3 archive and the branchDAM server. This document is the state-machine
writeup the code comments point at.

## Server-side prerequisite: read this first

**The whole offline flow depends on branchDAM having a `TIER0_LOCAL_STAGING` storage location
configured, and as of this PR that is NOT deployed anywhere real.** Verified directly, not
assumed:

- `branchdam`'s `config.example.yaml` and `config.dev.yaml` list `TIER0_LOCAL_STAGING` only in a
  comment enumerating valid `tier:` values -- neither has an actual entry.
- `ansible-playbooks`' `ansible/playbooks/docker/branchdam/templates/config.yaml.j2` says outright,
  in its own header comment: "No PROJECTS, TIER0_LOCAL_STAGING, or TIER1_LOCAL_SCRATCH entries —
  none are mounted." `compose.yml.j2` has no matching staging volume either.

Both are the plan's prerequisites **P1** (`branchdam` config) and **P2** (`ansible-playbooks`
mount), and neither is done. This PR implements and tests the offline flow against a **local**
branchDAM dev instance with a hand-added `TIER0_LOCAL_STAGING` location (see the M2 gate section
below) -- exactly as issue #4 anticipates ("implement and unit-test this PR against a local
branchdam dev instance where you add a Tier-0 location to config.yaml yourself"). Until P1/P2 land
in the real deployment, `ingest -offline` against the production server will resolve its Tier-0
container path to `ErrUnknownLocation`, burn three retries, and land the row's `node_created` step
in permanent backoff (see "Failure modes" below) -- not silently broken, but not usable in
production either.

## The three-step state machine

One `queue_nodes` row per ingested file, three independent status columns:

| Step | Values | What it means |
|---|---|---|
| `node_created` | `PENDING` → `SUBMITTED` | `EVENT_NODE_CREATED` was accepted (202) against the row's Tier-0 container path. `SUBMITTED` means *enqueued* server-side, not *applied* -- branchDAM's own drainer applies it asynchronously (contract gap 3: no feedback channel). |
| `archive_copy` | `PENDING` → `DONE` | The row's local edit copy has been copied into the final Tier-3 archive path and cache-defeating-verified against the hash computed at ingest time. |
| `rebase` | `PENDING` → `DONE` \| `FAILED` | `POST /api/v1/agent/rebase` moved the tracked node's `file_path` from the Tier-0 path to the real Tier-3 path. `FAILED` is terminal (a classified-fatal server error, e.g. the node was `ARCHIVED`) -- Drain never retries it. |

A `SIDECAR`-kind row (`.xmp`/`.srt`) only ever needs `archive_copy`: `node_created` and `rebase`
are `SKIPPED` at insert time, matching the online path's own "sidecars are copied but never
tracked as their own node" behavior.

## Ordering invariants

**1. `queue.db`'s row commits before any network mutation.** `Engine.ingestFileOffline` optionally
queries `GET /api/v1/agent/check-content` (pre-flight BLAKE3 content dedup), writes and verifies the
local copy, then `Store.InsertPending` -- the durability boundary -- *then* makes one best-effort
opportunistic `PostNodeCreated` attempt. A crash between the POST succeeding and the row being written
would leave a server-side node nothing local remembers, with no read endpoint to recover it; getting
the order backwards was the first thing worth getting wrong here, so it isn't.

**2. The archive copy never writes directly to the final path.** `CopyToArchive` writes to a
deterministically-named temp file in the destination directory and renames into place only after a
successful fsync+close. A crash mid-copy leaves an incomplete, never-renamed temp file that the
next attempt removes unconditionally before redoing the copy -- `DualWrite`'s own `O_EXCL` contract
(see `internal/ingest/writer.go`) would otherwise wedge a retry forever against a partial file
sitting at the final name.

**3. `POST /api/v1/agent/rebase` never fires until the archive copy is verified `DONE`.** This is
issue #4's headline property. `resolveRebaseTarget` (branchDAM server-side) refuses a rebase to a
read-only Tier-3 target with 400 unless the file already exists there -- `Drain`'s phase ordering
(node_created, then archive_copy, then rebase, each a separate pass over the row set) makes this
structural, not a race to avoid.

**4. Rebase additionally waits `MinRebaseDwell` (4s) after `node_created` was marked `SUBMITTED`.**
This is a *different* race than #3, and a more dangerous one because it fails silently instead of
with a 400: `EVENT_NODE_CREATED` is applied asynchronously by branchDAM's own event-queue drainer
(polling every 2s), while `/rebase` is synchronous. If `/rebase` reaches the server before the
Tier-0 create has been applied, `handleAgentRebase`'s unknown-`NodeUUID` branch takes over: it
inserts a **new** `media_nodes` row directly at the Tier-3 path using only what the rebase request
body carries (`fastHash`, size, mtime) -- no `fullHash`, no pHash, no promoted EXIF columns. The
Tier-0 event then arrives, `applyNodeCreated`'s own idempotency check finds the `NodeUUID` already
exists, and returns a silent no-op -- discarding every promoted column **permanently**, with
nothing to notice it happened (there is no agent-reachable lookup-by-path/uuid endpoint, contract
gap 5). `MinRebaseDwell` is a heuristic, not a proof, chosen as roughly 2x the server's own poll
interval to make this astronomically unlikely without depending on anything the server promises.

## Retry policy

Each of the three steps has its own attempt counter and `next_attempt_unix` column. A failure
schedules the next attempt at `2s * 2^(attempts-1)`, capped at 5 minutes (`backoffFor` in
`internal/ingest/drain.go`). `Drain` is stateless across calls -- it is the persisted
next-attempt timestamps that turn repeated calls (`queue-drain -watch`, a cron job, or the tray's
own `offline.drainIntervalSecs` timer, issue #32) into backoff rather than a tight retry loop.

`node_created` and `archive_copy` retry indefinitely -- every realistic failure mode on those two
(network down, NAS unmounted) is expected to eventually clear. `rebase` is the one step with a
terminal `FAILED` state: a classified-fatal server error (`internal/branchdam.HTTPError`'s
`Classification()`, e.g. `ErrArchivedNode`) means retrying can never succeed, and an operator has
to intervene -- check `rebase_last_error` in `queue.db`.

One `archive_copy` failure mode does NOT self-clear, and there's no terminal state to catch it: if
the local edit copy is deleted (by the user, by disk cleanup, by anything) before `archive_copy`
completes, `CopyToArchive`'s `open local copy` fails every single pass forever, backing off to the
5-minute cap and staying there indefinitely. `local_path` in `queue.db` is the thing to check if a
row's `archive_copy_attempts` climbs without bound.

## A note on the archive-copy temp file and concurrent scans

`CopyToArchive` writes to a dotfile-prefixed temp name (`.<final-name>.branchdam-agent-tmp`)
**inside** the Tier-3 archive directory during the copy window, then renames into place --
same-filesystem-atomic, and the reason a leftover from a killed copy is unambiguous on restart (see
ordering invariant #2). branchDAM's `internal/indexer.Walk` does not skip dotfiles or any other
name pattern -- it reports every regular file `fs.WalkDir` finds, full stop. A server-side scan of
the archive location that happens to run while a copy is in flight will index the temp file as its
own `media_nodes` row; once the rename completes, that row's path no longer exists on disk and it
becomes `MISSING` on the next scan sweep -- permanently, since rows are never deleted (see
branchDAM's own schema invariants). This is inert (no data loss, no wrong lineage -- just a
noise row that shows up as `MISSING` in an audit view) but real: avoid scanning a Tier-3 archive
location while `queue-drain` is actively copying into it, or expect the occasional orphaned
`MISSING` row named after this pattern.

## Reconnect handshake

`Drain` calls `POST /api/v1/agent/handshake` once per pass, log-only. Per the plan's contract gap
2, this is **not** a resume mechanism: `lastProcessedEventUuid`/`clientVersion` are accepted and
never read server-side, `pendingEventsCount` is server-global (not scoped to this agent), and
`acknowledgedEventUuid` only ever names a `PROCESSED` row (silently skips a `FAILED` one). A
handshake failure never aborts the rest of the pass -- `queue.db` is the only source of truth for
what's outstanding.

## Failure modes worth knowing about

- **Tier-0 location unconfigured server-side**: `EVENT_NODE_CREATED`'s `PostNodeCreated` call
  itself still succeeds (202 -- the payload isn't validated at enqueue time), but branchDAM's own
  drainer fails applying it (`ErrUnknownLocation`) and marks the *server-side* event `FAILED` after
  three retries, invisibly to the agent (contract gap 3). The agent's `queue.db` row stays
  `SUBMITTED` forever, `archive_copy` and eventually `rebase` still complete against the Tier-3
  path (a rebase of an *unknown* `NodeUUID`, since the Tier-0 create never actually landed, takes
  the `CREATED` branch -- see ordering invariant #4's failure mode, minus the "silent" part since
  in this case there was never a promoted-columns node to begin with). Net effect is worse than
  "just missing metadata": the `CREATED` branch's `InsertMediaNode` call sets `FullHash: nil`
  (the rebase request body carries `fastHash`, not `fullHash`), so `indexing_status` lands
  `INDEXED_SHALLOW` rather than `INDEXED_FULL` -- and per branchDAM's own Tier-3 heuristic
  resolver, a `INDEXED_SHALLOW` node with no `full_hash` is invisible to it, exactly the outcome
  plan decision 3 exists to prevent. Symptom to look for: `queue.db` rows stuck `SUBMITTED` +
  `DONE` + `DONE` whose `media_nodes` counterpart is `INDEXED_SHALLOW` with a `NULL full_hash`.
- **Genuinely offline for a long time**: every step backs off up to 5 minutes between attempts.
  `queue-drain -watch` (or repeated manual invocation, or the tray's own timer) is expected to be
  running continuously or periodically for the queue to actually drain once connectivity returns.

## Single writer once the tray is running (issue #32)

`branchdam-agent tray` now opens `queue.db` itself (when `offline.queueDbPath` is set) and runs its
own drain and prune passes on independent timers (`offline.drainIntervalSecs`,
`prune.intervalMinutes`) via `internal/tray.Runner.TriggerDrain`/`TriggerPrune` -- see
`cmd/branchdam-agent/queueagent.go`. With the tray running, it is the one long-lived process
holding `queue.db` open, the same role `queue-drain -watch` used to play alone. Running
`queue-drain -watch` (or `prune -watch`) *alongside* a running tray is redundant, not unsafe:
`queue.Store`'s WAL journal mode plus `busy_timeout=5000` (see `internal/queue/store.go`) make
concurrent writers from two processes correct, just pointless double work. There is no new lock
file or exclusion mechanism -- this is a documentation note, not an enforced invariant.

`Runner.TriggerDrain` and `Runner.TriggerPrune` use deliberately different locking (see their doc
comments in `internal/tray/tray.go`): drain uses its own dedicated mutex, entirely independent of
the ingest/self-update gate, so a 5s drain tick is never dropped during an ingest and never blocks
one either; prune shares that gate (via `TryLockIdle`) because it deletes from
`ingest.localEditRoot` while an ingest can be writing into it -- a hazard that only exists once
prune and ingest share a process, which `prune -watch` running standalone never had to guard
against.

## M2 gate: verifying this against a local branchDAM instance

1. Add a `TIER0_LOCAL_STAGING` location and a `TIER3_MASTER_ARCHIVE` (`readOnly: true`) location to
   a local branchDAM `config.yaml` (see `branchdam`'s `config.example.yaml` for the
   `storageLocations` shape). Both root paths just need to exist as empty directories.
2. With the branchDAM server **stopped**: `branchdam-agent ingest -config ... -card ... -offline`.
   Confirm the local copy exists, `queue.db` has one row with all three steps `PENDING`, and the
   archive path is untouched.
3. `curl -X POST .../api/v1/agent/rebase` for that row's `nodeUuid`/target path before starting the
   server (or before running `queue-drain`) -- confirm **400**, `no file exists there yet`.
4. Start the server. `branchdam-agent queue-drain -config ...`. Confirm `node_created` submits and
   `archive_copy` completes in the same pass, but `rebase` stays `PENDING` (dwell not yet elapsed).
5. Wait past `MinRebaseDwell` (4s) and run `queue-drain` again. Confirm `rebase` completes (200),
   and that the server's `media_nodes` row for that `nodeUuid` has `storage_location_id` pointing
   at the Tier-3 location, `file_path` at the Tier-3 path, and `full_hash`/`fast_hash` populated
   from the *original* `EVENT_NODE_CREATED` payload (proof the dwell gate worked -- a rebase that
   raced ahead of the Tier-0 create would show a `NULL full_hash` instead).

This exact sequence was run manually against a local `go run ./cmd/branchdam` during this PR's
development; see the PR description for the resulting `media_nodes` row.

## Direct HTTP Streaming Ingest vs. Offline Queue

Workstations connected over LAN or VPN can bypass local NAS storage mounts (`ArchiveRoot`) by using direct HTTP streaming ingest:

```sh
branchdam-agent ingest -config config.yaml -card /media/$USER/UNTITLED -upload
```

- **Online HTTP Streaming (`-upload`)**: Streams raw octets directly to `POST /api/v1/agent/upload`. The server persists the file into `TIER3_MASTER_ARCHIVE`, creates `media_nodes`, extracts metadata, and returns the canonical `relativePath` and BLAKE3 hash. The agent writes the local NVMe edit copy at `LocalEditRoot/relativePath` and cryptographically verifies the BLAKE3 checksum.
- **Offline Field Ingest (`-offline`)**: Used when disconnected from the server. Writes the local NVMe copy immediately and persists queue state to `queue.db`. On reconnect, `queue-drain` submits `EVENT_NODE_CREATED`, copies files to the archive, and rebases paths.

## Soft-Delete Trash Buffer Lifecycle

When an agent emits `EVENT_NODE_DELETED` (or when files are deleted from the DAM):
1. **Gallery Purge**: The asset is immediately removed from live UI indexing and Immich gallery libraries.
2. **Buffer Isolation**: The server relocates the master file into the `.trash/` directory under `TIER3_MASTER_ARCHIVE` rather than permanently unlinking it from disk.
3. **30-Day Safety Window**: The server's background prune worker retains `.trash/` files for 30 days (governed by `trash.retentionDays`), protecting against accidental deletions before final unlinking.
