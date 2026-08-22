// Package ingest is the SD-card ingest core (issue #2, M1): card detection,
// the one-read/two-write dual-copy writer, cache-defeating verified writes,
// metadata extraction matching branchDAM's own promoted-column set, DJI
// .srt GPS handling, and submission via internal/branchdam's client. Plain
// library code with no UI imports -- cmd/branchdam-agent's `ingest`
// subcommand is the only driver in this PR; a later tray PR is a second,
// thin driver over the same package (see the plan doc's M1 section).
package ingest

import (
	"strings"
	"time"
)

// DefaultPathTemplate matches the plan doc and issue #2's stated default.
const DefaultPathTemplate = "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}"

// TemplateVars are the placeholder values RenderPath substitutes.
type TemplateVars struct {
	CapturedAt   time.Time // falls back to the source file's mtime when no capturedAt was extracted
	CameraModel  string    // falls back to "unknown_camera" when empty (EXIF absent, or no exiftool)
	OriginalName string    // the source file's base name, unchanged
}

// sanitizeSegment replaces path separators and other characters that would
// either escape the destination root or produce an invalid path component
// on Windows (the plan's stated second target platform) with "_". Templates
// are config-supplied, not attacker-controlled, but CameraModel and
// OriginalName both originate from file content/names on a removable card,
// which this agent does not otherwise trust.
func sanitizeSegment(s string) string {
	if s == "" {
		return s
	}
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	return replacer.Replace(s)
}

// RenderPath expands tpl against vars, returning a slash-separated relative
// path (never absolute, never containing ".." after sanitization) suitable
// for joining under either ArchiveRoot or LocalEditRoot. Both destinations
// call this with the identical vars, which is what makes the local copy
// mirror the archive subtree by construction (issue #2's stated design).
func RenderPath(tpl string, vars TemplateVars) string {
	if tpl == "" {
		tpl = DefaultPathTemplate
	}
	cameraModel := vars.CameraModel
	if cameraModel == "" {
		cameraModel = "unknown_camera"
	}

	replacer := strings.NewReplacer(
		"{yyyy}", vars.CapturedAt.Format("2006"),
		"{mm}", vars.CapturedAt.Format("01"),
		"{dd}", vars.CapturedAt.Format("02"),
		"{camera_model}", sanitizeSegment(cameraModel),
		"{original_name}", sanitizeSegment(vars.OriginalName),
	)
	return replacer.Replace(tpl)
}
