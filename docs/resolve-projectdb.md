# Resolve database sync: workstation validation

The tray reads Resolve's local SQLite database or a configured PostgreSQL
Project Server without writing to either. The branchDAM server must expose
`POST /api/v1/agent/resolve-snapshot` before turning off `dryRun`; a 404 is
an upgrade-required error, not a successful legacy sync.

Set `integrations.resolvedb.enabled: true`, keep `dryRun: true` for the first
pass, and configure `integrations.nodeIndexPath` plus `pathRewrites` from
Resolve's original media paths to branchDAM container paths. An empty
`databaseUrl` probes the standard local paths on Windows, macOS, and Linux;
an explicit `file:` or `postgres://` URL overrides discovery. The connection
is forced read-only even if a SQLite URL requests a writable mode.

## Live acceptance checklist

Record the workstation OS, Resolve version, and database driver in issue
[#184](https://github.com/s3ntin3l8/branchdam-agent/issues/184). Run the
same sequence for an active local SQLite database and a PostgreSQL Project
Server when both are available:

1. In dry run, confirm timeline count, rewritten container paths, and node
   index hits; no server graph change should occur.
2. Turn dry run off and use **Sync now**. Confirm each timeline has one
   virtual Resolve node and each resolved media/timeline pair has one
   `PROJECT_SIDECAR` edge at confidence 1.00.
3. Sync unchanged again. Confirm `unchanged` increases with no duplicate
   edges or evidence changes.
4. Edit a clip's in-point/start/duration, rename a timeline, and use the
   same media file twice in one timeline. Confirm refreshed evidence holds
   both placements and the current timeline name.
5. Remove a clip from a timeline. Confirm its automatic edge remains in
   the database for audit with `is_active=0` but disappears from lineage.
   A human `CONFIRMED`/`REJECTED` edge must remain and produce a warning.
6. Temporarily remove the clip from the node index while it is still in
   Resolve. Confirm the existing edge is protected, not detached. Restore
   the index and sync again.
7. Restart the tray and repeat sync. Confirm the last-handshake timestamp
   and Resolve scope survive restart. Change the configured database only
   when intending to retire the previous scope.

A failed query, schema check, path lookup, server request, or partial
snapshot must never remove graph edges. The server commits a snapshot in
one transaction; runtime scope advances only after a successful response.
