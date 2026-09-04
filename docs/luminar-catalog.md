# Luminar Neo catalog — schema mapping and verification record

`internal/luminar` reads a Luminar Neo catalog to recover edit→source relationships and emit them
to branchDAM as `EVENT_EDGE_ATTACHED`. This document records what was actually established
against a real catalog (issue #34), replacing an earlier version built entirely on unverified
CoreData-based guesswork.

## Verification record

Checked 2026-08-28 against a real Luminar Neo catalog:

| | |
|---|---|
| App version | Luminar Neo, `settings.main_app_version = 6` |
| Catalog schema version | `settings.db_version = 155` |
| Platform | macOS |
| Catalog size | 995 images |
| Derivative pairs found | 2 (`_upscale`, `_panorama`) |

The catalog file itself is a bare SQLite database (`user_version` set, WAL journal mode), named
`*.luminarneo` (e.g. `Luminar Neo Catalog.luminarneo`), not a `catalog.db` inside a bundle
directory as Skylum's own support docs describe for the on-disk layout.

**If you validate against a different Luminar Neo version, add a row to the table above (or a new
one) and update `SchemaMappingVersion` below** rather than overwriting this record — the whole
point of stamping a version into every edge's `evidenceJson` is being able to tell which schema
guess produced which edge later.

## The load-bearing finding: Luminar Neo stores no relational edit→source lineage

This is the finding that shapes everything below, not just a corrected list of table names.
Checked directly against the real catalog:

- `image_user_attributes.origin_path_wide_ch` — **empty on all 995 rows**.
- `image_virtual_copy` (a non-destructive virtual-copy link table) — **0 rows**, and a virtual
  copy is not a second file on disk regardless.
- `img_history_states.data_wide_ch` (the non-destructive edit-instruction blob) — **length 0 on
  every row**; actual edit state lives in external `.arc`/`.tid`/`.msk`/`.lnp` resource files
  (`resources` table, `img_history_states_resources` join table), which are Luminar-internal
  sidecars, not user-facing output files branchDAM's graph could link to.
- No join table anywhere associates a derived file with its source image.

So a derived file that exists as a separate file on disk — the only kind branchDAM's graph can
link — is recoverable **only by filename convention**, never by reading a relationship out of the
catalog. This replaces an earlier query built on an Apple CoreData (`ZASSET`/`ZEDIT`/
`ZEXPORTPATH`) hypothesis that doesn't exist in Luminar Neo's real schema at all: that query
failed at SQL-prepare time against every real catalog, it never just returned wrong rows.

### Observed but deliberately unused signals

Recorded here so a future investigation doesn't re-derive and then re-discard the same three
things:

- **`img_history_states.name_wide_ch = "Sync with <filename>"`** — a localized UI string Luminar
  writes when its Sync feature links two originals (e.g. a DJI drone photo to its matching video
  frame). It links two *sources*, not an edit to its output, has no stable machine-readable form,
  and isn't a file-level relationship branchDAM's graph models. Not used.
- **A shared `.lnp` preset resource across `img_history_states_resources`** — several images'
  history states reference the same `.lnp` (Luminar preset) resource ID. This means "these edits
  used the same preset," not "these files are related." Not used.
- **`image_virtual_copy`** — 0 rows in this catalog, and even populated, a virtual copy is a
  non-destructive variant of the *same* file, not a second file on disk. Not used.

## The row-extraction query (`internal/luminar/query.go`)

`DefaultCatalogQuery` reads one row per (non-deleted) catalog image — it does **not** return
pairs, because the catalog doesn't store them:

```sql
SELECT
    CAST(i._id_int_64 AS TEXT)                                                             AS image_id,
    COALESCE(json_extract(NULLIF(v.info_wide_ch, ''), '$.kMountPointSerializationKey'), '') AS volume_mount,
    p.path_wide_ch                                                                          AS dir_path,
    i.path_wide_ch                                                                          AS file_name,
    COALESCE(ua.trash_bool, 0)                                                              AS trashed,
    COALESCE(x.camera_model_wide_ch, '')                                                    AS camera_model,
    COALESCE(NULLIF(x.date_time_int_64, 0), i.creation_date_int_64)                         AS capture_time
FROM images i
JOIN paths_images pi ON pi._val_id_int_64 = i._id_int_64
JOIN paths p         ON p._id_int_64      = pi._key_id_int_64
JOIN volumes v       ON v._id_int_64      = p.volume_id_int_64
LEFT JOIN image_user_attributes ua ON ua._out_id_int_64 = i._id_int_64
LEFT JOIN image_exiv_attributes x  ON x._out_id_int_64  = i._id_int_64
WHERE i.marked_to_delete_bool = 0
  AND i.deleted_at_int_64     = 0
  AND p.marked_to_delete_bool = 0
  AND v.marked_to_delete_bool = 0
ORDER BY i._id_int_64
```

Verified column shape (real schema, `_id_int_64`/`_wide_ch` convention, not CoreData's `Z`-prefix):

| Table | Column | Meaning |
|---|---|---|
| `images` | `_id_int_64` | Integer primary key |
| `images` | `path_wide_ch` | **Bare filename only** — never a path in this schema |
| `paths` | `path_wide_ch` | The image's containing directory, relative to its volume's mount point |
| `paths_images` | `_key_id_int_64` / `_val_id_int_64` | Join: directory → image. 1:1 in every image checked in the verified catalog, but **not schema-guaranteed** — its PK is the (dir, image) pair, not image alone, so nothing stops a future catalog from filing one image under two directories. `DefaultCatalogQuery` picks the lowest directory id deterministically (`MIN(_key_id_int_64)`) rather than trusting a plain join to stay 1:1; `image_user_attributes`/`image_exiv_attributes`, by contrast, both declare `_out_id_int_64 ... UNIQUE`, so those two `LEFT JOIN`s can never fan out regardless of catalog contents |
| `volumes` | `info_wide_ch` | JSON blob; `kMountPointSerializationKey` is the filesystem mount point (`/`, `/Volumes/Untitled`, ...) |
| `image_user_attributes` | `trash_bool` | 1 if the image is in Luminar's trash |
| `image_exiv_attributes` | `camera_model_wide_ch`, `date_time_int_64` | EXIF camera model and capture timestamp |

Gotchas hit during verification, each worth knowing before touching this query again:

- **`NULLIF(v.info_wide_ch, '')` is required.** The real catalog's own volume row 1 has an empty
  `info_wide_ch`; a bare `json_extract` on it raises `malformed JSON` and aborts the whole query.
- **Path assembly is Go's job, not SQL's.** Naive concatenation of mount (`/`) + directory
  (`Users/...`) yields `//Users/...`. `CatalogImage.FullPath()` (`internal/luminar/catalog.go`)
  uses `path.Join`.
- Only **macOS** path assembly (a POSIX mount point plus `/`-joined sub-paths) has been verified.
  A Windows Luminar Neo catalog's `volumes.info_wide_ch` shape is unobserved.
- **`path.Join` normalizes; `nodeindex.Resolve` doesn't.** `FullPath()` collapses `//`, cleans
  `..`, and strips a trailing `/`; the node-index JSON file is matched against verbatim, with no
  normalization on that side. The two only agree if the index file was itself built from
  already-normalized paths — true for every path form seen in the verified catalog, but worth
  checking first if a `-node-index` entry mysteriously never resolves.

## Pairing derived files (`internal/luminar/derive.go`)

Since the catalog carries no lineage, pairing is inferred entirely from filename convention, in
Go (`PairDerivatives`), not SQL — this makes the heuristic unit-testable and lets it reuse
ordinary string logic instead of encoding a guess inside a query no test can isolate.

An image whose stem ends in a known suffix is a **candidate**; if stripping the suffix produces a
stem matching **exactly one** other image in the catalog, that's the pair. Zero matches or more
than one is reported (`Ambiguity`, `Stats.NoSourceInCatalog`/`Stats.Ambiguous`) and never emitted
— pairing is a guess either way, but an *unambiguous* guess is a different risk profile from a
guess among multiple candidates.

The stem key deliberately ignores directory: two same-named source images in different folders
(e.g. `IMG_1767.jpeg` imported from two separate trips or cards) would make every derivative named
after either of them ambiguous, even though only one folder's derivative is really its match. Left
as-is — this is forward-looking, since the verified catalog had zero stem collisions across all
995 images (see the measurement below), and the fail-closed design means a collision produces zero
wrong edges, only a missed one. If cross-directory same-name collisions ever show up in practice,
key on `(DirPath, base stem)` instead of stem alone.

`DefaultDerivativeSuffixes = ["_upscale", "_panorama"]` — the only two suffixes confirmed against
the real catalog, both written by Luminar Neo features that produce a **new file** added back to
the library:

| derived | source |
|---|---|
| `IMG_1767_upscale.jpg` (9216×16380) | `IMG_1767.jpeg` (1536×2730) |
| `DJI_20260824170503_0008_D_PANORAMA.tiff` | `DJI_20260824170503_0008_D.JPG` |

**Zero-false-positive measurement.** Run across all 995 images in the real catalog with a
deliberately *wider* candidate list than what ships (also including `_hdr`, `_enhanced`,
`_denoise`, `_sky-enhance`, `_relight`, `_sharpen`, `_composite`, `_merge`, `_stack`), exactly
these 2 candidates matched, each resolving to exactly 1 unambiguous source: **zero false
positives, zero stem collisions across the whole catalog.** This measurement is what justifies
holding `Confidence` at 0.89 (see below) rather than lowering it now that pairing is
filename-inferred instead of read from the catalog.

EXIF (camera model, capture time) is corroboration recorded as `cameraModelMatch`/
`captureTimeMatch` in `evidenceJson`, **never a pairing gate**: the upscale pair agrees on both
camera and capture time, but the panorama pair agrees only on camera — Neo finalizes a stitched
panorama well after the source shots were taken. Gating on capture-time agreement would have
silently dropped a true pair.

`PairDerivatives` deliberately does **not** call `internal/naming.Stem`. That package is a
byte-for-byte port of branchDAM's own `naming.Stem` under an explicit invariant (see this repo's
AGENTS.md); its role-suffix pattern has no `_upscale`/`_panorama` and must never gain
Luminar-specific suffixes just because this package needs something similar-shaped. `derive.go`
has its own small, local `stem` helper instead.

Trashed images are **not** filtered out of pairing or emission: the real upscale pair's edit side
(`IMG_1767_upscale.jpg`) is trashed in Luminar, and the underlying file may well still exist on
disk. If it doesn't, `nodeindex` simply fails to resolve it and the pair lands in
`Stats.EditUnresolved` like any other missing file — `sourceTrashed`/`editTrashed` ride along in
`evidenceJson` so this is visible, not silent.

### How to correct this against a different catalog

Two independent knobs now, not one, because pairing moved out of SQL:

1. **Row extraction** (`DefaultCatalogQuery`): run
   `branchdam-agent luminar-sync -catalog <path> -dump-schema` against your catalog, diff the
   real table/column names against the table above, and either edit `query.go` directly (bump
   `SchemaMappingVersion` alongside it) or write a corrected query to a file and pass
   `-query-file <path>` — no code change or release required.
2. **Derivative pairing** (`DefaultDerivativeSuffixes`): if your catalog's Luminar Neo version
   writes derived files with a different suffix, pass
   `-derivative-suffixes "_yoursuffix,_another"` — also no code change or release required. This
   is deliberately separate from `-query-file`: it corrects only the suffix list, not row
   extraction, and speculative suffixes stay out of the shipped default on purpose (see the
   zero-false-positive measurement above — it was run to justify exactly two, not more).

## Evidence stamping and the tier-1 promotion path

Every emitted edge's `evidenceJson` includes `schemaMapping: "neo-db155"` (`luminar.SchemaMappingVersion`
— **not** `"verified"` unqualified: lineage itself is inferred from filenames, not read from the
catalog, and only one Luminar Neo version has been checked), the catalog path, both file paths,
both catalog row IDs, `matchRule: "filename-suffix"`, the matched `suffix`, `cameraModelMatch`/
`captureTimeMatch`, and `sourceTrashed`/`editTrashed`. This exists so that if this pairing rule
turns out to be wrong for some catalog, every edge it produced can be found and corrected later —
the same pattern as branchDAM's own `#132`/`00006` migration for a different resolver's mis-tuned
matcher. Per issue #6/#34 and the plan: promoting this resolver to `tier: 1, confidence: 1.00` is
**out of scope**, and should only happen after a period of the tier-2 edges being reviewed and
consistently confirmed correct in branchDAM's audit queue — with its own data-correction migration
on the `branchdam` side for edges already written at the lower tier, not a silent confidence bump.

## Node resolution scope (issue #6's required decision)

Issue #6 frames the choice as "scope to agent-ingested masters only" vs. "use a lookup-by-path
endpoint if one exists by the time this lands." Neither framing is quite right once you read
branchDAM's actual `applyEdgeAttached` (`internal/agent/drainer.go`): it resolves **both**
`sourceNodeUuid` and `targetNodeUuid` via `GetMediaNodeByUUID`, and an unresolvable node ID is not
classified as a fatal error by `internal/branchdam/errors.go`'s fatal/transient substring
matching — it would silently burn the full retry budget and land the event `FAILED`, with no
feedback channel back to the agent.

**The actual v1 scope: an edge is only emitted when BOTH the source master's path and the edit
output's path resolve to a known `nodeUuid`.** In practice that means both files must have been
agent-ingested (or otherwise recorded) — a Luminar edit whose export lands somewhere only a normal
branchDAM server scan will ever see is silently *not* a candidate today, since nothing agent-side
knows that scan's resulting `nodeUuid`.

`internal/nodeindex` is the seam this is built behind: a `Resolver` interface
(`Resolve(path) (nodeUuid string, ok bool, err error)`) with one implementation today, `FileIndex`
— a flat, hand-editable/script-generated JSON file mapping absolute file paths to `nodeUuid`
strings. A pair with either endpoint unresolved is skipped, not silently dropped: `luminar-sync`
logs every skip and reports skip counts (plus ambiguous/no-source/unresolvable-path candidate
counts) in its summary line, so "0 edges emitted" is distinguishable from "N pairs found, all
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
