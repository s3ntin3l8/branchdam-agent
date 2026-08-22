"""Unit tests for the pure manifest-writing logic in branchdam_render_hook.py.

No Resolve dependency -- these exercise `build_manifest`/`manifest_filename`/
`write_manifest` directly against fixture source-clip lists, per issue #5's
acceptance criteria. The Resolve-API-calling half of the module (`get_resolve`,
`watch_render_queue`, ...) is not covered here; see README.md's "Manual testing"
section for how that half was verified instead.
"""

from __future__ import annotations

import json

import pytest
from branchdam_render_hook import (
    MANIFEST_SUFFIX,
    MediaReference,
    build_manifest,
    manifest_filename,
    write_manifest,
)

FIXTURE_REFS = [
    MediaReference(raw_path=r"D:\Footage\Day1\A001_C001_0817.ARW", role="media"),
    MediaReference(raw_path=r"D:\Footage\Day1\A001_C001_0817_proxy.mp4", role="proxy"),
    MediaReference(raw_path=r"D:\Footage\Day1\A001_C001_0817.wav", role="audio"),
]


def test_build_manifest_matches_schema() -> None:
    manifest = build_manifest("Autumn Campaign", FIXTURE_REFS)

    assert manifest["version"] == "1.0"
    assert manifest["project_name"] == "Autumn Campaign"
    assert manifest["media_references"] == [
        {"raw_path": r"D:\Footage\Day1\A001_C001_0817.ARW", "role": "media"},
        {"raw_path": r"D:\Footage\Day1\A001_C001_0817_proxy.mp4", "role": "proxy"},
        {"raw_path": r"D:\Footage\Day1\A001_C001_0817.wav", "role": "audio"},
    ]


def test_build_manifest_never_emits_export_role() -> None:
    manifest = build_manifest("Autumn Campaign", FIXTURE_REFS)

    roles = {ref["role"] for ref in manifest["media_references"]}
    assert "export" not in roles
    assert roles <= {"media", "proxy", "audio"}


def test_build_manifest_rejects_export_role_explicitly() -> None:
    refs = [
        *FIXTURE_REFS,
        MediaReference(raw_path="Z:\\Renders\\Final.mp4", role="export"),
    ]

    with pytest.raises(ValueError, match="not allowed"):
        build_manifest("Autumn Campaign", refs)


def test_build_manifest_never_has_files_key() -> None:
    # branchDAM's parser treats every entry in a top-level "files" array as an
    # implicit media-role reference too -- a second, easy-to-miss path to the
    # same backwards render-is-its-own-parent edge that dropping role: "export"
    # alone does not close.
    manifest = build_manifest("Autumn Campaign", FIXTURE_REFS)

    assert "files" not in manifest


def test_build_manifest_requires_project_name() -> None:
    with pytest.raises(ValueError, match="project_name"):
        build_manifest("", FIXTURE_REFS)


def test_build_manifest_requires_version() -> None:
    with pytest.raises(ValueError, match="version"):
        build_manifest("Autumn Campaign", FIXTURE_REFS, version="")


def test_build_manifest_dedupes_repeated_clips() -> None:
    refs = FIXTURE_REFS + FIXTURE_REFS  # a 40-cut timeline reusing the same clips
    manifest = build_manifest("Autumn Campaign", refs)

    assert len(manifest["media_references"]) == len(FIXTURE_REFS)


def test_build_manifest_skips_empty_paths() -> None:
    refs = [*FIXTURE_REFS, MediaReference(raw_path="", role="media")]
    manifest = build_manifest("Autumn Campaign", refs)

    assert len(manifest["media_references"]) == len(FIXTURE_REFS)


@pytest.mark.parametrize(
    ("render_name", "expected"),
    [
        ("Autumn_Campaign_Final.mp4", "Autumn_Campaign_Final.dam.json"),
        ("Autumn_Campaign_Final", "Autumn_Campaign_Final.dam.json"),
        ("Already.dam.json", "Already.dam.json"),
        ("weird.name.mov", "weird.name.dam.json"),
    ],
)
def test_manifest_filename_always_ends_in_dam_json(
    render_name: str, expected: str
) -> None:
    result = manifest_filename(render_name)

    assert result.endswith(MANIFEST_SUFFIX)
    assert result == expected


def test_manifest_filename_rejects_empty_input() -> None:
    with pytest.raises(ValueError):
        manifest_filename("")


def test_write_manifest_writes_valid_json_next_to_export(tmp_path) -> None:
    out_path = write_manifest(
        tmp_path, "Autumn_Campaign_Final.mp4", "Autumn Campaign", FIXTURE_REFS
    )

    assert out_path.parent == tmp_path
    assert out_path.name == "Autumn_Campaign_Final.dam.json"
    assert out_path.name.endswith(MANIFEST_SUFFIX)

    loaded = json.loads(out_path.read_text(encoding="utf-8"))
    assert loaded["version"] == "1.0"
    assert loaded["project_name"] == "Autumn Campaign"
    assert all(ref["role"] != "export" for ref in loaded["media_references"])
    assert "files" not in loaded


def test_write_manifest_raw_paths_are_not_translated(tmp_path) -> None:
    # raw_path must be passed through verbatim, in whatever form Resolve reports
    # it (Windows or macOS) -- no container-path translation happens client-side.
    mac_refs = [
        MediaReference(
            raw_path="/Volumes/Footage/Day1/A001_C001_0817.mov", role="media"
        )
    ]
    out_path = write_manifest(tmp_path, "Render.mov", "Autumn Campaign", mac_refs)

    loaded = json.loads(out_path.read_text(encoding="utf-8"))
    assert loaded["media_references"][0]["raw_path"] == (
        "/Volumes/Footage/Day1/A001_C001_0817.mov"
    )
