"""DaVinci Resolve post-render `.dam.json` hook.

Installed into Resolve's ``Scripts/Utility/`` directory (see ``README.md`` in this
directory for install steps). Launched from Resolve's Workspace > Scripts menu, it
watches the render queue and, for each job that completes, writes a
``<render_name>.dam.json`` sidecar next to the rendered export, listing the
timeline's *source* clips (never the render itself) so branchDAM's
``ProjectSidecarResolver`` can link the export back to its masters/proxies/audio.

Module layout, deliberately:

* The top half (``MediaReference`` through ``write_manifest``) is pure Python with
  no Resolve dependency at all. It is fully unit-tested (see
  ``test_branchdam_render_hook.py``) against fixture clip lists.
* The bottom half (``get_resolve`` through ``main``) is a thin wrapper that calls
  Resolve's scripting API (``GetProjectManager``, ``GetRenderJobList``,
  ``GetItemListInTrack``, ``GetClipProperty``, ...). It cannot be exercised by CI —
  there is no Resolve install available in this pipeline — and is therefore kept as
  small and mechanical as possible: fetch data from the API, hand it to the pure
  functions above. See the README's "Manual testing" section for how this half was
  actually verified.

Design decision (do not re-litigate; see branchDAM issue #233 / this repo's issue
#5): the manifest lists only the render's *sources* (``media``/``proxy``/``audio``),
never an ``export`` entry for the render itself. branchDAM's ``ProjectSidecarResolver``
treats every listed reference as a *parent* of the manifest's own node at confidence
1.00 (never downgradable later) — listing the render would make it a parent of
itself.
"""

from __future__ import annotations

import json
import os
import sys
import time
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from pathlib import Path

# Roles the branchDAM `.dam.json` parser accepts from this hook. `export` is
# intentionally absent -- see the module docstring.
ALLOWED_ROLES = frozenset({"media", "proxy", "audio"})

MANIFEST_SUFFIX = ".dam.json"

MANIFEST_VERSION = "1.0"

# Resolve's render job status strings that mean "done, one way or another" -- a
# render queue watcher should stop polling a job once it reaches one of these.
_TERMINAL_JOB_STATUSES = frozenset({"Complete", "Failed", "Cancelled"})


@dataclass(frozen=True)
class MediaReference:
    """One source clip referenced by a render's timeline.

    ``raw_path`` is the workstation's own path, exactly as Resolve reports it
    (Windows ``D:\\...`` or macOS ``/Volumes/...``). Do **not** translate it to a
    server-container path here -- branchDAM's server-side ``pathRewrites`` config
    does that translation on ingestion. This is the opposite convention from the
    Go agent's ``EVENT_NODE_CREATED`` payloads, which *do* carry container paths;
    see README.md's "Path conventions" section.
    """

    raw_path: str
    role: str


# --------------------------------------------------------------------------- #
# Pure, independently-testable manifest logic. No Resolve dependency below.
# --------------------------------------------------------------------------- #


def build_manifest(
    project_name: str,
    media_references: Iterable[MediaReference],
    version: str = MANIFEST_VERSION,
) -> dict:
    """Build the `.dam.json` manifest dict for one completed render.

    Deduplicates references on ``(raw_path, role)`` (a timeline commonly reuses the
    same clip across many cuts) while preserving first-seen order. Raises
    ``ValueError`` if ``project_name``/``version`` are empty (branchDAM's parser
    hard-rejects a manifest missing either -- see ``internal/projectfile/dam_json.go``)
    or if any reference uses a role outside ``ALLOWED_ROLES``.

    Deliberately never emits a top-level ``files`` key: branchDAM's parser turns
    every entry in ``files`` into a ``media``-role reference too, which is a second,
    easy-to-miss path to the same backwards render-is-its-own-parent edge that
    omitting ``role: "export"`` alone would not close.
    """
    if not project_name:
        raise ValueError("project_name must not be empty")
    if not version:
        raise ValueError("version must not be empty")

    seen: set[tuple[str, str]] = set()
    ordered_refs: list[MediaReference] = []
    for ref in media_references:
        if not ref.raw_path:
            continue
        if ref.role not in ALLOWED_ROLES:
            raise ValueError(
                f"role {ref.role!r} is not allowed in a render-hook manifest "
                f"(allowed: {sorted(ALLOWED_ROLES)}); the render's own export must "
                "never be listed"
            )
        key = (ref.raw_path, ref.role)
        if key in seen:
            continue
        seen.add(key)
        ordered_refs.append(ref)

    return {
        "version": version,
        "project_name": project_name,
        "media_references": [
            {"raw_path": ref.raw_path, "role": ref.role} for ref in ordered_refs
        ],
    }


def manifest_filename(render_output_name: str) -> str:
    """Derive the sidecar filename for a render, always ending in ``.dam.json``.

    branchDAM's parser registry matches the compound suffix ``.dam.json``
    specifically, not bare ``.json`` (``internal/projectfile/registry.go``), so a
    plain ``os.path.splitext`` + ``".json"`` would silently produce a file the
    server never picks up.
    """
    if not render_output_name:
        raise ValueError("render_output_name must not be empty")
    if render_output_name.lower().endswith(MANIFEST_SUFFIX):
        return render_output_name
    stem, _ext = os.path.splitext(render_output_name)
    stem = stem or render_output_name
    return f"{stem}{MANIFEST_SUFFIX}"


def write_manifest(
    target_dir: str | os.PathLike[str],
    render_output_name: str,
    project_name: str,
    media_references: Sequence[MediaReference],
    version: str = MANIFEST_VERSION,
) -> Path:
    """Write the manifest for one completed render and return the written path."""
    manifest = build_manifest(project_name, media_references, version)
    out_path = Path(target_dir) / manifest_filename(render_output_name)
    out_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return out_path


# --------------------------------------------------------------------------- #
# Thin Resolve-API-calling wrapper. Cannot be exercised in CI -- see README.md's
# "Manual testing" section for how this half was actually verified.
# --------------------------------------------------------------------------- #


def get_resolve():  # pragma: no cover
    """Return Resolve's scripting entry point.

    A script launched from Resolve's Workspace > Scripts menu commonly runs with a
    `resolve` global already injected into its namespace. That is not guaranteed
    for every Resolve version/launch path, so fall back to the documented explicit
    connection (`DaVinciResolveScript.scriptapp("Resolve")`), which requires
    `RESOLVE_SCRIPT_API`/`RESOLVE_SCRIPT_LIB`/`PYTHONPATH` to be set as Resolve's
    own docs describe for out-of-process scripts.
    """
    injected = globals().get("resolve") or getattr(
        sys.modules.get("__main__"), "resolve", None
    )
    if injected is not None:
        return injected

    import DaVinciResolveScript as dvr_script

    return dvr_script.scriptapp("Resolve")


def get_clip_references(timeline) -> list[MediaReference]:  # pragma: no cover
    """Collect source clip references from a timeline's video and audio tracks.

    Video track items map to role ``media``; if the clip has a non-empty
    ``"Proxy Media Path"`` clip property, that path is also emitted with role
    ``proxy``. Audio track items map to role ``audio``. The render itself is never
    a track item, so this can never accidentally include the export.
    """
    refs: list[MediaReference] = []

    video_tracks = timeline.GetTrackCount("video") or 0
    for track_index in range(1, video_tracks + 1):
        for item in timeline.GetItemListInTrack("video", track_index) or []:
            mp_item = item.GetMediaPoolItem()
            if mp_item is None:
                continue
            file_path = mp_item.GetClipProperty("File Path")
            if file_path:
                refs.append(MediaReference(raw_path=file_path, role="media"))
            proxy_path = mp_item.GetClipProperty("Proxy Media Path")
            if proxy_path:
                refs.append(MediaReference(raw_path=proxy_path, role="proxy"))

    audio_tracks = timeline.GetTrackCount("audio") or 0
    for track_index in range(1, audio_tracks + 1):
        for item in timeline.GetItemListInTrack("audio", track_index) or []:
            mp_item = item.GetMediaPoolItem()
            if mp_item is None:
                continue
            file_path = mp_item.GetClipProperty("File Path")
            if file_path:
                refs.append(MediaReference(raw_path=file_path, role="audio"))

    return refs


def find_timeline_by_name(project, timeline_name: str):  # pragma: no cover
    """Look up a project timeline by name (render jobs record only the name)."""
    count = project.GetTimelineCount() or 0
    for index in range(1, count + 1):
        timeline = project.GetTimelineByIndex(index)
        if timeline is not None and timeline.GetName() == timeline_name:
            return timeline
    return None


def process_completed_job(project, job: dict) -> Path | None:  # pragma: no cover
    """Write the `.dam.json` sidecar for one completed render job, if possible."""
    timeline_name = job.get("TimelineName")
    target_dir = job.get("TargetDir")
    output_filename = job.get("OutputFilename")
    if not (timeline_name and target_dir and output_filename):
        return None

    timeline = find_timeline_by_name(project, timeline_name)
    if timeline is None:
        return None

    refs = get_clip_references(timeline)
    project_name = project.GetName() or timeline_name
    return write_manifest(target_dir, output_filename, project_name, refs)


def watch_render_queue(
    resolve, poll_interval_seconds: float = 2.0
) -> None:  # pragma: no cover
    """Poll the current project's render queue and write a manifest per completion.

    Resolve's scripting API has no render-queue-complete callback/event to hook
    (confirmed against the published API docs while writing this script) -- render
    state is only observable by polling `GetRenderJobList`/`GetRenderJobStatus`.
    This loop runs for the lifetime of the script (stop it from Resolve's Scripts
    menu, or Ctrl-C from a console-launched instance).
    """
    project_manager = resolve.GetProjectManager()
    processed: set[str] = set()

    while True:
        project = project_manager.GetCurrentProject()
        if project is not None:
            for job in project.GetRenderJobList() or []:
                job_id = job.get("JobId")
                if not job_id or job_id in processed:
                    continue
                status = project.GetRenderJobStatus(job_id) or {}
                job_status = status.get("JobStatus")
                if job_status == "Complete":
                    process_completed_job(project, job)
                    processed.add(job_id)
                elif job_status in _TERMINAL_JOB_STATUSES:
                    # Failed/Cancelled: nothing rendered, nothing to link.
                    processed.add(job_id)
        time.sleep(poll_interval_seconds)


def main() -> None:  # pragma: no cover
    resolve = get_resolve()
    if resolve is None:
        print("branchdam_render_hook: could not connect to Resolve", file=sys.stderr)
        return
    try:
        watch_render_queue(resolve)
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":  # pragma: no cover
    main()
