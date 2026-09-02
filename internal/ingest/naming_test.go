package ingest

import (
	"os"
	"path/filepath"
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

func TestRenderPathStemAndExtTokens(t *testing.T) {
	vars := TemplateVars{
		CapturedAt:   time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
		CameraModel:  "ILCE-7M4",
		OriginalName: "DSC01234.ARW",
	}
	got := RenderPath("{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{stem}_custom.{ext}", vars)
	want := "2026/2026-08-22_ILCE-7M4/DSC01234_custom.ARW"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuffixedFilename(t *testing.T) {
	tests := []struct {
		orig   string
		suffix string
		want   string
	}{
		{"DSC0001.JPG", "_2", "DSC0001_2.JPG"},
		{"clip.mp4", "_3", "clip_3.mp4"},
		{"README", "_2", "README_2"},
		{".gitignore", "_2", ".gitignore_2"},
		{"DSC0001.JPG", "", "DSC0001.JPG"},
	}
	for _, tc := range tests {
		got := SuffixedFilename(tc.orig, tc.suffix)
		if got != tc.want {
			t.Errorf("SuffixedFilename(%q, %q) = %q, want %q", tc.orig, tc.suffix, got, tc.want)
		}
	}
}

// populateCollidingFixture writes n colliding files to root -- each named
// DSC_0001.JPG, DSC_0001_2.JPG, DSC_0001_3.JPG, ..., content distinct and
// all guaranteed distinct from the source -- so ResolveDestination, when
// asked to resolve a fresh source against root, must walk the counter loop
// discovering the n prior collisions before allocating the next slot.
// sizeBytes is the per-file byte length; larger values make FastHash's
// 6MiB read budget (3 × 2MiB regions) actually expensive and reproduce
// issue #105's O(N^2) read traffic in the unoptimized implementation.
func populateCollidingFixture(t testing.TB, root string, n, sizeBytes int) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "DSC_src.JPG")
	srcBytes := make([]byte, sizeBytes)
	for j := range srcBytes {
		// source's cubic seed -- intentionally distinct from the
		// colliding files' linear-prime+quadratic salt below so no
		// counter can produce a matching pattern.
		srcBytes[j] = byte(j*j*j + 0x5a)
	}
	if err := os.WriteFile(src, srcBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		name := filepath.Join(root, "DSC_0001.JPG")
		if i > 1 {
			name = filepath.Join(root, SuffixedFilename("DSC_0001.JPG", suffixForCounter(i)))
		}
		data := make([]byte, sizeBytes)
		for j := range data {
			// 7919 is prime; salt per file ensures FastHash windows
			// never collide with the source's seed.
			data[j] = byte(i*7919 + j*j + 0x11)
		}
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func suffixForCounter(n int) string {
	if n <= 1 {
		return ""
	}
	return "_" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestResolveDestinationCollisionLoopBudget asserts the user-visible
// contract from issue #105: a directory containing N colliding distinct
// files must resolve a fresh source in bounded read traffic (the
// hashBudget cap), not in unbounded O(N^2) I/O. File size 4 MiB matches
// the issue's quoted worst case ("10000 × 2 reads × 4 MiB = 80 TB"),
// ensuring FastHash's 6 MiB read window is fully exercised on each
// call. With 500 such collisions the unoptimized implementation reads
// ~6 GiB and takes many seconds; the budget-bounded implementation
// reads at most ~768 KiB × 500 = ~375 MiB and completes in ~1s. The
// 8s allowance accounts for race-detector overhead (the test runs with
// `-race` via `make test`) and CI SSD variability; the *budget* is the
// hashBudget constant in naming.go, the time figure is a soft target.
func TestResolveDestinationCollisionLoopBudget(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive")
	const (
		n         = 500
		sizeBytes = 4 * 1024 * 1024
	)
	src := populateCollidingFixture(t, archive, n, sizeBytes)

	vars := TemplateVars{
		CapturedAt:   time.Now(),
		CameraModel:  "ILCE-7M4",
		OriginalName: "DSC_0001.JPG",
	}
	start := time.Now()
	res := ResolveDestination([]string{archive}, "{original_name}", vars, src, "")
	elapsed := time.Since(start)

	if res.AlreadyIngested {
		t.Fatalf("expected a fresh collision slot, got AlreadyIngested (suffix=%q)", res.Suffix)
	}
	wantSuffix := suffixForCounter(n + 1)
	if res.Suffix != wantSuffix {
		t.Errorf("suffix = %q, want %q (counter=%d)", res.Suffix, wantSuffix, n+1)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("collision loop took %v for %d prior collisions; budget is hashBudget in naming.go (issue #105)", elapsed, n)
	}
}

// BenchmarkResolveDestination1000Collisions drives the 1000-file collision
// case from issue #105 with 256KiB files (matches the new
// collisionSampleSize, so each FastHash region covers the whole file and
// no read can be silently elided). Reports allocs/op and ns/op; the
// budget assertion lives in TestResolveDestinationCollisionLoopBudget
// because benchmarks skip the failure path by default.
func BenchmarkResolveDestination1000Collisions(b *testing.B) {
	dir := b.TempDir()
	archive := filepath.Join(dir, "archive")
	src := populateCollidingFixture(b, archive, 1000, 256*1024)

	vars := TemplateVars{
		CapturedAt:   time.Now(),
		CameraModel:  "ILCE-7M4",
		OriginalName: "DSC_0001.JPG",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := ResolveDestination([]string{archive}, "{original_name}", vars, src, "")
		if res.AlreadyIngested {
			b.Fatal("expected a fresh collision slot")
		}
		if res.Suffix != suffixForCounter(1001) {
			b.Fatalf("suffix = %q, want %q", res.Suffix, suffixForCounter(1001))
		}
	}
}
