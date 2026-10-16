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
| `archive_copy` | `PENDING` → `DONE` \| `FAILED` | The row's local edit copy has been copied into the final Tier-3 archive path and cache-defeating-verified against the hash computed at ingest time. `FAILED` is terminal: a different file already occupies the archive path (it is never overwritten), or the local copy no longer matches its queued hash. A row whose archive copy failed never reaches `rebase`. |
| `rebase` | `PENDING` → `DONE` \| `FAILED` | `POST /api/v1/agent/rebase` moved the tracked node's `file_path` from the Tier-0 path to the real Tier-3 path. `FAILED` is terminal (a classified-fatal server error, e.g. the node was `ARCHIVED`) -- Drain never retries it on its own; `queue-drain -retry-failed` re-queues it. |

A `SIDECAR`-kind row (`.xmp`/`.srt`) only ever needs `archive_copy`: `node_created` and `rebase`
are `SKIPPED` at insert time, matching the online path's own "sidecars are copied but never
tracked as their own node" behavior.

### Online ingest rows (`origin = ONLINE`)

When `offline.queueDbPath` is set, online ingest (`IngestCard`, the tray's normal path and plain
`ingest`) also writes one row per media file, with `origin = ONLINE`. By then both copies are
written and verified and `EVENT_NODE_CREATED` already targets the Tier-3 archive path, so the row
starts with `archive_copy = DONE` and `rebase = SKIPPED`; only `node_created` is left. The row is
inserted before the POST (ordering invariant 1 below applies to online ingest too). If the POST is
not accepted -- server down, a 5xx, the ingest cancelled -- the file is reported as queued, not
failed, and `Drain`'s `node_created` phase retries it with the same `nodeUuid` and event UUID. An
`ONLINE` row is pending until the registration is accepted (it counts in the "uploaded, confirming
archive" bucket meanwhile) and is never a `prune` candidate: online ingest keeps its edit copies.
Rows written before schema v3 are `OFFLINE`.

Two more online-ingest rules close the "copied but never registered" gap without a queue:

- The archive path's container mapping (`pathMappings`) is resolved before the copy, so a missing
  mapping fails the file while both destinations are still empty.
- A re-run that finds identical copies already at both destinations no longer takes that as proof
  of registration. It skips only when a queue row claims the copies or `check-content` finds a live
  (`ACTIVE` or `HIDDEN`) node with their BLAKE3 (hashed from the local copy, not the card, and looked
  up by full hash directly, with the fast hash and size alongside; the same per-pass rules as the
  fresh-copy dedup apply -- a 400 retries as a plain full-hash lookup, a transport error, 404/501,
  401/403 or 429 stops asking for the rest of the pass). A live node at the file's own archive path is its own
  registration, not a duplicate. Otherwise it verifies the archive copy against that hash --
  honouring `ingest.requireUnbuffered` -- and registers the copies (the server ignores an
  `EVENT_NODE_CREATED` whose `full_hash` a live node already has). A fresh copy that fails a
  required unbuffered verify is removed, so the next run copies and checks again instead of finding
  it "already ingested".

## Ordering invariants

**1. `queue.db`'s row commits before any network mutation.** `Engine.ingestFileOffline` writes the
local copy under its temp name (hashing it in flight), optionally queries `GET
/api/v1/agent/check-content` with those hashes (fast-hash pre-screen, then a full-hash confirm only
on a possible match; a confirmed live duplicate removes the temp copy and queues nothing), verifies
the copy, then `Store.InsertPending` -- the durability boundary -- *then* makes one best-effort
opportunistic `PostNodeCreated` attempt. A crash between the POST succeeding and the row being written
would leave a server-side node nothing local remembers, with no read endpoint to recover it; getting
the order backwards was the first thing worth getting wrong here, so it isn't.

**2. Nothing is ever written to, or deleted from, a final name except by renaming the agent's own
complete copy into a free one.** Both offline copies -- the local edit copy at ingest and the archive
copy at drain -- are written to a deterministically-named temp file next to their destination
(`.<final-name>.branchdam-agent-tmp`, plus the extension again for the local copy) and renamed into
place, refusing to replace anything already there, only once complete: the archive copy after a
successful fsync+close *and* a BLAKE3 match against the queued hash, the local copy only after its
queue row is committed. A crash mid-copy leaves only a temp file, which the next attempt removes before
redoing the copy -- the agent's temp names are the only files it deletes on its own initiative. In
particular:

- Offline ingest never removes a file it finds at a destination name. An unclaimed file there (an
  online ingest's edit copy, a user's file) keeps its name and the new file takes the next suffix; an
  identical file there is trusted as "already ingested" only if a queue row claims it or the reachable
  archive holds the same file. When the archive is reachable at ingest time, a name a different
  archive file already holds is not handed out either.
- `CopyToArchive` never removes a file at the archive path that it did not just place there. A
  matching file means the copy is already done; a different one fails the row's `archive_copy` with
  `FAILED` and an `archive_copy_last_error` naming the path -- move or rename one of the two files.
- A resumed row whose local copy never reached its final name (a crash between the queue insert and
  the rename) is completed on the next ingest of the same card: from the verified temp copy, or by
  copying the file from the card again.

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

**5. A re-run resumes a file only when its queue row still describes it.** `ingestFileOffline`
looks up the newest row for the file's source path and treats it as a resume (no copy, no new
`NodeUUID`, reported as *resumed* rather than *queued*) only when the row's `size_bytes` and
`mtime_unix` match the file on the card. Finished rows are kept for months (see "Row garbage
collection") and cameras restart their file counters after a format, so a second card mounted at the same path presents the same source paths
with different files; those are ingested as new work under their own suffixed destination. The
size and mtime come from the stat the ingest already does -- the card is still read once, by the
copy (invariant 1). A file that ends up neither copied, resumed nor deliberately skipped counts as
failed, so the card is never reported safe to eject on its strength.

## Retry policy

Each of the three steps has its own attempt counter and `next_attempt_unix` column. A failure
schedules the next attempt at `2s * 2^(attempts-1)`, capped at 5 minutes (`backoffFor` in
`internal/ingest/drain.go`). `Drain` is stateless across calls -- it is the persisted
next-attempt timestamps that turn repeated calls (`queue-drain -watch`, a cron job, or the tray's
own `offline.drainIntervalSecs` timer, issue #32) into backoff rather than a tight retry loop.

`node_created` retries indefinitely, and so does `archive_copy` for every failure that can clear by
itself (network down, NAS unmounted). `archive_copy` turns terminal `FAILED` only for the cases
retrying can't fix -- a different file at the archive path, a local copy that no longer matches its
queued hash (edited in place before or during the copy: the copy hashes the bytes as it reads them,
so the edited bytes never reach the archive name), or an archive copy that reads back with the
wrong hash on two attempts in a row (once is retried: a flaky mount can do that; twice means the
archive isn't storing the bytes it is given) -- and all show up in the queue's failed count with
the reason in `archive_copy_last_error`. A file already at the archive path with exactly the local
copy's size counts as a read-back mismatch, not a conflict: it is typically the agent's own copy,
left in place when an earlier attempt's post-rename read-back errored. Unlike a copy the attempt
just placed, it is never removed, since the agent can't prove it wrote it. `rebase` also has a terminal `FAILED`
state, reached only when the server's rebase handler itself refused the request for a reason that
won't change on its own (`internal/branchdam.HTTPError`'s `Classification()`: a problem+json
400/404/409/410/413/422 such as an `ARCHIVED` node, a `fullHash` conflict or a read-only non-Tier-3
target, or a recognised fatal `code`); 401/403/408/429, every 5xx and transport errors stay
`PENDING` with backoff. `node_created` has no terminal state: even a registration the server
rejects permanently (a 4xx it will never accept) stays `PENDING` and is retried with backoff, so
such a row is found by its growing `node_created_attempts` and the server's reason in
`node_created_last_error`.

What the operator does depends on which step failed:

- **`rebase` FAILED** -- read `rebase_last_error`, fix the server-side cause (e.g. un-archive the
  node, or correct the storage location), then run `queue-drain -retry-failed`.
- **`archive_copy` FAILED** -- read `archive_copy_last_error`. For a conflict, move or rename the
  file already at the archive path (it is never overwritten); for a read-back mismatch, check the
  archive mount and move or rename any file the error says was left in place; for a local-copy
  mismatch, restore the local edit copy to the bytes that were ingested (or re-ingest from the
  card). Then run `queue-drain -retry-failed`.

`queue-drain -retry-failed` puts every failed archive copy and rebase back to `PENDING`
(`queue.Store.ResetFailed`) and drains.

One `archive_copy` failure mode does NOT self-clear, and there's no terminal state to catch it: if
the local edit copy is deleted (by the user, by disk cleanup, by anything) before `archive_copy`
completes, `CopyToArchive`'s `open local copy` fails every single pass forever, backing off to the
5-minute cap and staying there indefinitely. `local_path` in `queue.db` is the thing to check if a
row's `archive_copy_attempts` climbs without bound. If the card still holds the file, ingesting it
again restores the local copy (see ordering invariant #2's resume note).

## Archive-copy parity with online ingest

The archive copy `Drain` places is held to the same standard as an online `DualWrite` master: it
gets the local copy's mtime (which ingest set to the card file's own) before the rename, so the
Tier-3 file carries the capture-side mtime rather than the moment the NAS came back, and the
archive directory is fsynced after the rename so the new name survives a power cut. The offline
local copy's rename into place gets the same directory fsync. A failed `Chtimes` or directory fsync
is logged at WARN and does not fail the step: the bytes are in place and verified.

## Row garbage collection

`Drain` sweeps `queue.db` for rows nothing needs any more, at most once a day per process
(`queue.GCInterval`; the sweep stats each candidate's local copy, which is fine daily and wasteful
on every 5-second tick). A row is deleted only when it **finished successfully** (terminal and not
`FAILED`), was **created more than 90 days ago** (`queue.GCRetention`), and its **local edit copy is
gone**. So:

- pending rows (Drain's work list) and `FAILED` rows (waiting for an operator or `queue-drain
  -retry-failed`) are never collected, however old;
- a row whose local copy still exists is kept: it is what `prune` checks before deleting that copy,
  and what marks the name as the agent's own for later offline ingests. With `prune` disabled,
  rows are therefore collected only after the operator deletes the local copies themselves --
  deliberately conservative;
- the 90 days protect resume detection: a card ingested offline and re-inserted within that window
  is still recognised as a resume even if its local copies were pruned.

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

If the response carries a `pendingRotation` hint (the server telling this device about a newer
API key), `internal/branchdam.Client.Handshake` logs it at `Warn` -- see issue #235. Nothing acts
on it yet: this agent has no device-pairing flow, so it never sends `currentKeyId` on the request,
and the server never sends the hint back without one. Rotating the stored key in response is a
separate, not-yet-implemented mechanism.

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
