# Luminar `catalog.db` — schema mapping and confidence

`internal/luminar` reads Skylum Luminar's `catalog.db` to recover edit→source relationships and
emit them to branchDAM as `EVENT_EDGE_ATTACHED`. This document records what was actually
established during research for issue #6, at what confidence, and exactly where the guessed
schema lives in code so it's easy to correct once someone can validate it against a real catalog.

**Read this before trusting anything downstream of `internal/luminar/query.go`.** The schema
mapping below is a best-effort reconstruction from indirect evidence, not a verified fact. That's
why every edge this reader emits lands at `tier: 2, confidence: 0.89` — strictly below branchDAM's
tier-2 auto-accept threshold (0.90) — in the human audit queue, not auto-committed. See issue #6
and `branchdam`'s `.claude/plans/can-we-walk-through-sharded-lighthouse.md` (M4 section) for the
full reasoning behind that choice.

## What was and wasn't found

No real Luminar `catalog.db` file was available during development of this reader, and no
authoritative schema documentation exists publicly. Specifically checked and ruled out:

- **Skylum's own support docs** (`support.skylum.com/how-to-use-luminar-neo/catalog/...` and the
  equivalent `-for-desktop` pages) describe the Catalog only at a user-facing level: it's a folder
  (not a single file) created automatically on first install, containing "a database of your
  images and their locations, thumbnails ... and a history of adjustments." Edits are described as
  non-destructive, stored "as instructions in a database rather than changing the actual files."
  No table names, column names, file names, or format (SQLite vs. something else) are stated on
  these pages for Luminar Neo specifically.
- **No published plugin, export, or developer API** describes Luminar's data model. Searches for a
  Luminar Neo SDK / plugin API surfaced only end-user integration docs (the Lightroom plugin, the
  general plugin *installation* process) — nothing that documents an internal schema.
- **No GitHub repository, forensic write-up, or community post** was found that dumps or documents
  Luminar Neo's `catalog.db` table structure specifically.
- **The legacy Luminar 4 catalog format IS attested, indirectly**, by third-party file-format
  reference sites (fileinfo.com, serptools.github.io's `.luminar` page): a Luminar 4 catalog
  (`Luminar Catalog.luminar`) is stated to be "a SQLite database" used "to retrieve the TIFF and
  STATE files stored within a catalog and associate them with one another, so edited images appear
  correctly." This confirms the *shape* of the problem (a SQLite catalog associating an original
  with a derived edit-state file) for a related, earlier product generation, but is not a
  Luminar-Neo-specific schema and predates at least one full rewrite (Luminar 4 → Neo).
- **Luminar's history as a CoreData-backed macOS app** (the product originated as Macphun's Luminar
  for macOS, later ported to Windows and rewritten as Neo) is general background knowledge, not
  something re-verified against a specific build during this research pass. CoreData's SQLite
  store has a well-known naming convention — entity tables and attribute columns prefixed `Z`
  (`ZASSET`, `ZFILEPATH`, ...), a synthetic `Z_PK` integer primary key, and `Z_ENT`/`Z_PRIMARYKEY`
  bookkeeping tables — which is what `DefaultEditSourceQuery` (below) is built against, on the
  theory that Luminar kept its original CoreData-shaped schema across the Windows port and the Neo
  rewrite. **This is the single weakest link in the whole mapping**: it is plausible, not
  confirmed, and Luminar Neo (as opposed to legacy Luminar) may not even use CoreData at all.

**Bottom line confidence: LOW.** Treat every identifier in the query below as a hypothesis.

## The schema mapping (`internal/luminar/query.go`)

`DefaultEditSourceQuery` is the *only* place this guessed schema knowledge lives:

```sql
SELECT
    asset.ZFILEPATH   AS source_path,
    edit.ZEXPORTPATH  AS edit_path,
    CAST(asset.Z_PK AS TEXT) AS source_row_id,
    CAST(edit.Z_PK  AS TEXT) AS edit_row_id
FROM ZEDIT edit
JOIN ZASSET asset ON asset.Z_PK = edit.ZASSET
WHERE edit.ZEXPORTPATH IS NOT NULL AND edit.ZEXPORTPATH != ''
```

Assumed shape:

| Table | Assumed column | Assumed meaning |
|---|---|---|
| `ZASSET` | `Z_PK` | Integer primary key of an imported master/original image |
| `ZASSET` | `ZFILEPATH` | Absolute path to the original file on disk |
| `ZEDIT` | `Z_PK` | Integer primary key of one non-destructive edit/version |
| `ZEDIT` | `ZASSET` | Foreign key back to `ZASSET.Z_PK` — the edit's source image |
| `ZEDIT` | `ZEXPORTPATH` | Absolute path of the *exported* output file, if the edit has ever been exported; `NULL`/empty if the edit exists only as in-app adjustment instructions with no file on disk yet |

Only edits with a non-empty `ZEXPORTPATH` are selected — an edit that has never been exported has
no second file for branchDAM's graph (which links files, not in-catalog edit state) to attach an
edge to.

### How to correct this once a real catalog is available

This query is deliberately isolated and runtime-overridable, not hardcoded as unavoidable:

1. Run `branchdam-agent luminar-sync --catalog <path to a real catalog.db> --dump-schema` against
   a real Luminar catalog. This prints every object in `sqlite_master` (table/index/view/trigger
   name plus its original `CREATE` statement) — the actual table and column names, whatever they
   turn out to be.
2. Compare that output against `DefaultEditSourceQuery` above.
3. Either fix `internal/luminar/query.go`'s `DefaultEditSourceQuery` directly (bump
   `luminar.SchemaMappingVersion` alongside it — see below), or write a corrected query to a file
   and pass `--query-file <path>` without touching Go code or cutting a release at all.

No other file needs to change to correct the schema mapping — `internal/luminar/catalog.go`'s
`EditSourcePairs` only requires the query to return exactly four columns in order (`source_path`,
`edit_path`, `source_row_id`, `edit_row_id`); it has no other schema knowledge.

## Evidence stamping and the tier-1 promotion path

Every emitted edge's `evidenceJson` includes `schemaMapping: "v1-unverified"`
(`luminar.SchemaMappingVersion`), the catalog path, both file paths, and both catalog row IDs. This
exists so that if this schema guess turns out to be wrong for some or all catalogs, every edge it
produced can be found and corrected later — the same pattern as branchDAM's own `#132`/`00006`
migration for a different resolver's mis-tuned matcher. Per issue #6 and the plan: promoting this
resolver to `tier: 1, confidence: 1.00` is **out of scope for this PR**, and should only happen
after a period of the tier-2 edges being reviewed and consistently confirmed correct in branchDAM's
audit queue — with its own data-correction migration on the `branchdam` side for edges already
written at the lower tier, not a silent confidence bump.

## Node resolution scope (issue #6's required decision)

Issue #6 frames the choice as "scope to agent-ingested masters only" vs. "use a lookup-by-path
endpoint if one exists by the time this lands." Neither framing is quite right once you read
branchDAM's actual `applyEdgeAttached` (`internal/agent/drainer.go`): it resolves **both**
`sourceNodeUuid` and `targetNodeUuid` via `GetMediaNodeByUUID`, and an unresolvable node ID is not
classified as a fatal error by `internal/branchdam/errors.go`'s fatal/transient substring
matching — it would silently burn the full retry budget and land the event `FAILED`, with no
feedback channel back to the agent (`branchdam-agent`'s plan, contract gap 3).

**The actual v1 scope: an edge is only emitted when BOTH the source master's path and the edit
output's path resolve to a known `nodeUuid`.** In practice that means both files must have been
agent-ingested (or otherwise recorded) — a Luminar edit whose export lands somewhere only a normal
branchDAM server scan will ever see is silently *not* a candidate today, since nothing agent-side
knows that scan's resulting `nodeUuid`. This is the real, narrower version of the "no
agent-reachable lookup-by-path endpoint" gap the plan documents as prerequisite P5.

`internal/nodeindex` is the seam this is built behind: a `Resolver` interface
(`Resolve(path) (nodeUuid string, ok bool, err error)`) with one implementation today, `FileIndex`
— a flat, hand-editable/script-generated JSON file mapping absolute file paths to `nodeUuid`
strings. This exists because `branchdam-agent`'s M1 (SD-card ingest core) and M2 (offline
`queue.db`) milestones, which are the eventual real source of this mapping (an agent always knows
the `nodeUuid` it minted for its own ingested files), had not landed when this reader was built.
Once `queue.db` exists, it becomes a second `Resolver` implementation with the same one-method
interface — nothing above `internal/nodeindex` needs to change. A pair with either endpoint
unresolved is skipped, not silently dropped: `luminar-sync` logs every skip and reports
skip counts in its summary line, so "0 edges emitted" is distinguishable from "N pairs found, all
skipped."

## Why `?mode=ro`, never `?immutable=1`

Verified empirically in `internal/luminar/catalog_wal_test.go`'s `TestModeROSeesLiveWAL`: with a
WAL-mode SQLite database whose writer connection is held open (the realistic case — a catalog
someone has open in Luminar right now) and a row written but not yet checkpointed out of the `-wal`
file, `?mode=ro` sees the row correctly and still refuses a write. `?immutable=1` against the same
live-WAL database failed outright in this repo's pinned `modernc.org/sqlite` version (`no such
table` — it doesn't see the WAL-only schema/data at all), which is the same "silently missing the
newest data" failure mode issue #6 flags, just manifesting as a harder error rather than a quiet
stale read in this specific case. Either way, the conclusion holds: never use `?immutable=1` against
a catalog file that might be open elsewhere.
