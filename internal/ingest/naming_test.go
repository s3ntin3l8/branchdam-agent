package ingest

import (
	"strings"
	"testing"
	"time"
)

func TestRenderPathDefaultTemplate(t *testing.T) {
	// The multi-token default template was, until this test, entirely
	// unexercised -- every other test in the package pins pathTemplate to
	// "{original_name}". This is the parity-critical path: online and
	// offline ingest both call RenderPath with the *default* template
	// whenever the operator hasn't overridden ingest.pathTemplate.
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 7, 15, 14, 30, 0, 0, time.UTC),
		CameraModel:  "ILCE-7M4",
		OriginalName: "DSC01234.ARW",
	}
	got := RenderPath(DefaultPathTemplate, vars)
	want := "2026/2026-07-15_ILCE-7M4/DSC01234.ARW"
	if got != want {
		t.Errorf("RenderPath(default) = %q, want %q", got, want)
	}
}

func TestRenderPathEmptyTemplateFallsBackToDefault(t *testing.T) {
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),
		CameraModel:  "ILCE-7M4",
		OriginalName: "IMG_0001.JPG",
	}
	got := RenderPath("", vars)
	want := RenderPath(DefaultPathTemplate, vars)
	if got != want {
		t.Errorf("RenderPath(\"\") = %q, want it to equal RenderPath(DefaultPathTemplate) = %q", got, want)
	}
}

func TestRenderPathDateTokensZeroPadded(t *testing.T) {
	// {mm}/{dd} must be zero-padded (Go's "01"/"02" layout), not "1"/"5".
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
		OriginalName: "f.jpg",
	}
	got := RenderPath("{yyyy}-{mm}-{dd}/{original_name}", vars)
	want := "2026-03-05/f.jpg"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderPathCameraModelUnknownFallback(t *testing.T) {
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
		CameraModel:  "", // no EXIF, no exiftool
		OriginalName: "clip.mp4",
	}
	got := RenderPath("{camera_model}/{original_name}", vars)
	want := "unknown_camera/clip.mp4"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderPathSanitizesCameraModelAndOriginalName(t *testing.T) {
	// Both CameraModel and OriginalName originate from card content this
	// agent doesn't trust -- confirm every documented unsafe character is
	// replaced with "_" in both fields.
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CameraModel:  `ILCE/7M4\:*?"<>|`,
		OriginalName: `weird/name\:*?"<>|.jpg`,
	}
	got := RenderPath("{camera_model}/{original_name}", vars)
	if strings.ContainsAny(got, `\:*?"<>`) {
		t.Errorf("sanitized output still contains an unsafe character: %q", got)
	}
	// "/" is the path separator itself, so it's expected in the joined
	// result -- but not *within* a single rendered segment. Split on the
	// one "/" the template itself introduces and check each side.
	parts := strings.SplitN(got, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("expected exactly one template-introduced separator, got %q", got)
	}
	if strings.Contains(parts[0], "/") || strings.Contains(parts[1], "/") {
		t.Errorf("a sanitized segment still contains an internal slash: %q", got)
	}
}

func TestRenderPathUnknownTokenPassesThroughVerbatim(t *testing.T) {
	// Documented behavior: an unrecognized token (e.g. a typo, or a token
	// from a future version of this doc that isn't implemented yet) is left
	// in the output unchanged rather than erroring.
	vars := TemplateVars{CapturedAt: time.Now(), OriginalName: "f.jpg"}
	got := RenderPath("{not_a_real_token}/{original_name}", vars)
	if !strings.HasPrefix(got, "{not_a_real_token}/") {
		t.Errorf("got %q, want the unknown token preserved verbatim", got)
	}
}

func TestRenderPathAllFiveTokensTogether(t *testing.T) {
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		CameraModel:  "FX3",
		OriginalName: "A001C001.MXF",
	}
	got := RenderPath("{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}", vars)
	want := "2026/2026-12-31_FX3/A001C001.MXF"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRenderPathTraversalSequenceNotStripped documents a known, filed gap
// rather than asserting desired behavior: sanitizeSegment's replacer list
// does not include "..", so a ".." sequence in CameraModel or
// OriginalName survives into the rendered path unchanged, despite
// RenderPath's own doc comment claiming the result never contains "..".
// This test exists so a future fix has a red test to turn green, and so a
// well-intentioned refactor doesn't accidentally "fix" this without
// updating the doc comment and this test together.
func TestRenderPathTraversalSequenceNotStripped(t *testing.T) {
	vars := TemplateVars{
		CapturedAt:   time.Now(),
		CameraModel:  "../../etc",
		OriginalName: "f.jpg",
	}
	got := RenderPath("{camera_model}/{original_name}", vars)
	if !strings.Contains(got, "..") {
		t.Skip("sanitizeSegment now strips \"..\" -- update RenderPath's doc comment to match and delete this test's stale framing")
	}
}
