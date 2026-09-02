package ingest

import (
	"path/filepath"
	"strings"
)

// shouldSkipByName reports whether path's basename matches an
// OS-metadata entry the walk unconditionally excludes (issue #100):
//
//   - "Thumbs.db" / "thumbs.db" (case-insensitive): Windows thumbnail
//     cache, written by Explorer into every folder it touches.
//   - "System Volume Information" / lowercase: Windows NTFS metadata
//     folder; not a regular file, but recorded as a walk entry some
//     platforms (and the path-equality test in this package) need to
//     explicitly exclude.
//   - any basename starting with ".": ".DS_Store" (macOS Finder),
//     "._IMG_0001.ARW" (macOS AppleDouble sidecar), ".Trashes" (macOS),
//     and every other dotfile that has no business in a card's media
//     tree.
//
// The boolean return is paired with a non-empty reason whenever true;
// both fields are no-op zero values on false. Order in this function is
// "name first" (cheap, allocation-free), per the issue's "skip by-name
// first, then extension filter" guidance.
func shouldSkipByName(path string) (bool, string) {
	base := filepath.Base(path)
	lower := strings.ToLower(base)

	switch {
	case lower == "thumbs.db":
		return true, "OS metadata: Thumbs.db (Windows thumbnail cache)"
	case lower == "system volume information":
		return true, "OS metadata: System Volume Information (Windows NTFS metadata)"
	case strings.HasPrefix(base, "."):
		return true, "OS metadata: dotfile " + base
	}
	return false, ""
}

// shouldSkipByExtension reports whether ext should be skipped under the
// given AllowedExtensions allowlist. ext is the file's extension
// WITHOUT a leading dot (matching extNoDot's convention, so a
// comparison-ready value is already in hand at the call site). allowed
// is treated as a case-insensitive set, normalized to lowercase
// comparison and stripped of any leading dots -- an operator can write
// "JPG", "jpg", or ".JPG" in YAML and all three resolve to the same
// match against a file's "jpg" extension.
//
// An empty ext (a file with no extension, e.g. "LICENSE", "README",
// or a media file the camera wrote without one) is NOT skipped by an
// allowlist: the allowlist only filters KNOWN-EXTENSION files whose
// extension isn't on the list. The intent of the allowlist is
// "ingest only media types the operator enumerated", and the
// unstated corollary is "files that have no extension at all
// haven't been positively identified as a media type yet, so
// surfacing them via ingestFile (which will run isImageExt/
// isVideoExt and produce a real FileResult) is the safer default
// than silently dropping them". Returns false when allowed is
// nil/empty (the "accept all" default -- callers can short-circuit
// on len(allowed) == 0, but this function also behaves correctly if
// called directly, so unit tests don't have to mirror that
// optimization).
func shouldSkipByExtension(allowed []string, ext string) bool {
	if len(allowed) == 0 {
		return false
	}
	if ext == "" {
		return false
	}
	lowerExt := strings.ToLower(ext)
	for _, a := range allowed {
		if strings.ToLower(strings.TrimPrefix(a, ".")) == lowerExt {
			return false
		}
	}
	return true
}
