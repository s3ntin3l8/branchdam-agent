# DaVinci Resolve post-render `.dam.json` hook

`branchdam_render_hook.py` is a script for DaVinci Resolve's `Scripts/Utility/`
directory. It watches the render queue and, for each render that completes, writes
a `<render_name>.dam.json` manifest **next to the rendered export**, listing the
timeline's source clips so branchDAM's `ProjectSidecarResolver` can link the export
back to its masters, proxies, and audio at Tier-1 confidence.

See `branchdam`'s `docs/dam-manifest.md` for the manifest schema and
`internal/graph/resolvers.go`'s `ProjectSidecarResolver` for how the server
consumes it.

## Installation

1. Copy `branchdam_render_hook.py` into Resolve's `Scripts/Utility/` directory:
   - Windows: `%APPDATA%\Blackmagic Design\DaVinci Resolve\Support\Fusion\Scripts\Utility\`
   - macOS: `/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility/`
   - Linux: `/opt/resolve/Fusion/Scripts/Utility/` (or your install's equivalent)
2. In Resolve, open the project you want to watch, then
   **Workspace > Scripts > Utility > branchdam_render_hook**.
3. The script runs a poll loop for the life of the session (see "How it works"
   below) — leave it running while you render. Stop it from the Scripts menu, or
   with Ctrl-C if launched from a console.

No install step writes anything outside the render's own output directory; no
`PROJECTS` storage location is needed on the branchDAM server side for this to
work — the `.dam.json` lands in the already-scanned Tier-2 exports location.

## How it works

Resolve's scripting API has **no render-queue-complete callback or event to hook**
— confirmed against the published API docs while writing this script; render state
is only observable by polling `Project.GetRenderJobList()` /
`Project.GetRenderJobStatus(jobId)`. Despite this repo's issue title, this script is
therefore a poll loop, not an event hook in the literal sense: it watches
`GetRenderJobList()`, and the moment a job's status flips to `"Complete"`, it reads
that job's timeline's source clips and writes the manifest. Jobs that end
`"Failed"`/`"Cancelled"` are skipped (nothing rendered, nothing to link).

The module is split in two, deliberately:

- `MediaReference`, `build_manifest`, `manifest_filename`, `write_manifest` are
  pure Python with no Resolve dependency, and are the part covered by
  `test_branchdam_render_hook.py`.
- `get_resolve`, `get_clip_references`, `find_timeline_by_name`,
  `process_completed_job`, `watch_render_queue`, `main` are a thin wrapper around
  Resolve's scripting API (`GetProjectManager`, `GetRenderJobList`,
  `GetItemListInTrack`, `GetMediaPoolItem`, `GetClipProperty`, ...). They contain no
  manifest-writing logic of their own — they fetch data from the API and hand it to
  the pure functions above.

## Manifest content

Per branchDAM issue #233 / this repo's issue #5 (a decided design fork, not a free
choice): the manifest lists **only** the render's sources, with roles `media`,
`proxy`, and `audio`. It **never** lists the render itself (no `role: "export"`
entry, and no top-level `files` array either — branchDAM's parser treats every
`files` entry as an implicit `media`-role reference, which is a second path to the
same problem).

Why: branchDAM's `ProjectSidecarResolver` makes every listed reference a *parent*
of the manifest's own node at confidence 1.00, and a confidence-1.00 edge is never
downgraded by a later resolver. Listing the render would make the render a parent
of its own manifest — backwards, and permanently uncorrectable.

## Path conventions — read this before wiring up anything else in this repo

`raw_path` values in the manifest are the **workstation's own** paths, exactly as
Resolve reports them (Windows `D:\Footage\...` or macOS `/Volumes/Footage/...`).
This script does **not** translate them to server-container paths — branchDAM's
server-side `pathRewrites` config (see `branchdam`'s `docs/configuration.md` and
`docs/project-paths.md`) does that translation when the `.dam.json` is ingested.

**This is the opposite convention from this repo's own Go agent** (`internal/`,
unrelated to this directory): `EVENT_NODE_CREATED` payloads sent over the agent
protocol must carry server-container paths, not workstation paths. Conflating the
two is the single most confusable thing between this script and the rest of the
repo — if you're wiring up something new that touches both, check which side of
that line it's on.

## Manual testing

This script cannot be exercised in this repo's CI: there is no DaVinci Resolve
install available on any CI runner, and Resolve's `DaVinciResolveScript` module is
only importable from inside a Resolve installation (or with
`RESOLVE_SCRIPT_API`/`RESOLVE_SCRIPT_LIB`/`PYTHONPATH` pointed at one). What CI
*does* run is `hooks/resolve/test_branchdam_render_hook.py`, which covers the pure
manifest-writing logic (schema shape, no `export` role, no `files` key, `.dam.json`
suffix, path-passthrough, dedup) against fixture clip lists — see
`.github/workflows/ci-python.yml`.

The Resolve-API-calling half (`get_resolve`, `get_clip_references`,
`find_timeline_by_name`, `process_completed_job`, `watch_render_queue`) was
verified by reading Resolve's published scripting API documentation and
community references (there was no real Resolve install available while writing
this), specifically:

- `Project.GetRenderJobList()` / `Project.GetRenderJobStatus(jobId)` (status dict
  with a `JobStatus` key; `"Complete"`/`"Failed"`/`"Cancelled"` are the terminal
  values used here) — no event/callback API exists alongside these.
- `Timeline.GetTrackCount(trackType)` / `Timeline.GetItemListInTrack(trackType, index)`
  (1-based track index) for enumerating video/audio track items.
- `TimelineItem.GetMediaPoolItem()` → `MediaPoolItem.GetClipProperty("File Path")` /
  `GetClipProperty("Proxy Media Path")` for source and proxy paths.
- `ProjectManager.GetCurrentProject()`, `Project.GetTimelineCount()` /
  `Project.GetTimelineByIndex(index)` / `Timeline.GetName()` for resolving a render
  job's `TimelineName` back to a timeline object.

Community documentation on the exact return-dict key sets is inconsistent between
sources (e.g. `GetRenderJobList` vs. an older `GetRenderJobs` name in some
references), so treat this as **not independently verified against a real Resolve
session** — before relying on this in production, run it against an actual Resolve
install and confirm the render job dict's keys (`JobId`, `TimelineName`,
`TargetDir`, `OutputFilename`) and `GetClipProperty`'s property-name strings match
what's coded here, and adjust if your Resolve version differs. `get_resolve()`
defensively tries an already-injected `resolve` global first (the common case when
launched from Resolve's own Scripts menu) before falling back to the documented
explicit `DaVinciResolveScript.scriptapp("Resolve")` connection, since public
examples disagree on which one a menu-launched Utility script gets by default.
