package phash

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/exiftool"
)

// adversarialPaths mirrors branchDAM's own probe/argv_test.go fixture list --
// paths crafted to look like exiftool flags, proving a path that could be
// misread as a flag is always protected by "--".
var adversarialPaths = []string{
	"/normal/path/DSC001.ARW",
	"-overwrite_original",
	"-TAG=value.jpg",
	"--exec=rm -rf /",
	"-b",
	"",
	// A filename containing a newline would split into two argfile lines
	// in the pooled, line-oriented "-stay_open" protocol -- letting the
	// text after the newline be read as its own argument -- even though
	// it doesn't start with "-" itself. See exiftool.NeedsSeparator.
	"/normal/path/foo\n-overwrite_original",
	// exiftool's argfile reader skips a line starting with "#" as a
	// comment and strips leading whitespace from any line -- neither is
	// an injection risk, but both silently corrupt the request unless
	// routed around the argfile protocol entirely. See NeedsSeparator.
	"#not-a-comment.jpg",
	"  /leading/whitespace.jpg",
}

var tagAssignmentRe = regexp.MustCompile(`^-[A-Za-z0-9:_-]+=`)

// TestPreviewArgsShape proves previewArgs' security property: any path
// that could be misread as a flag or tag assignment (i.e. starts with "-")
// is always preceded by a "--" separator, and the path is always the last
// argument. A path that does NOT start with "-" is never at risk of being
// misparsed regardless of "--", so previewArgs is free to omit it there --
// see exiftool.NeedsSeparator's doc comment for why that's not just an
// optimization but a requirement for the pooled path.
func TestPreviewArgsShape(t *testing.T) {
	for _, path := range adversarialPaths {
		args := previewArgs(path)
		if len(args) == 0 || args[len(args)-1] != path {
			t.Fatalf("previewArgs(%q) = %v, want to end with the path", path, args)
		}

		sawSep := false
		for _, a := range args[:len(args)-1] {
			if a == "--" {
				sawSep = true
				continue
			}
			if sawSep {
				continue
			}
			if tagAssignmentRe.MatchString(a) {
				t.Errorf("previewArgs(%q) contains a tag assignment before --: %v", path, args)
			}
		}
		if exiftool.NeedsSeparator(path) && !sawSep {
			t.Errorf("previewArgs(%q) looks like a flag but has no -- separator: %v", path, args)
		}
	}
}

// TestPreviewArgsRequestsAllTagsAtOnce proves the "single invocation" part
// of the design: one previewArgs call requests every tag in previewTags,
// not just one -- the property that turns 3 exiftool invocations per RAW
// file into 1.
func TestPreviewArgsRequestsAllTagsAtOnce(t *testing.T) {
	args := previewArgs("/x.arw")
	for _, tag := range previewTags {
		found := false
		for _, a := range args {
			if a == "-"+tag {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("previewArgs = %v, missing -%s", args, tag)
		}
	}
}

// TestExtractDirectDecodeSkipsExiftool proves the fast path never touches
// the pool at all: pool is nil (which would panic on any Execute call),
// and Extract must still succeed (and match hashing's own golden vector)
// because gradient.png decodes directly.
func TestExtractDirectDecodeSkipsExiftool(t *testing.T) {
	got, err := Extract(context.Background(), nil, "../hashing/testdata/gradient.png")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got == nil {
		t.Fatal("Extract returned nil hash for a directly-decodable PNG")
	}
	const want = -6161209626521968640 // same golden vector as hashing.TestPerceptualHashGoldenVectors
	if *got != want {
		t.Errorf("Extract(gradient.png) = %d, want %d", *got, want)
	}
}

// TestExtractNoExiftoolNoDirectDecode proves the (nil, nil) "nothing
// usable" contract: a file that fails direct decode, with pool nil (the
// exiftool-unavailable case), must return (nil, nil), not an error.
func TestExtractNoExiftoolNoDirectDecode(t *testing.T) {
	dir := t.TempDir()
	notAnImage := filepath.Join(dir, "notanimage.arw")
	if err := os.WriteFile(notAnImage, []byte("this is not image data"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Extract(context.Background(), nil, notAnImage)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("Extract with no exiftool and no direct decode = %v, want nil", got)
	}
}

// fakeExecutor is a canned executor standing in for a pooled exiftool
// process: it returns a fixed JSON row (as pool.Execute would) without
// spawning anything, and records the args it was called with so a test can
// assert on the request shape.
type fakeExecutor struct {
	row       map[string]any
	calls     [][]string
	returnErr error
}

func (f *fakeExecutor) Execute(_ context.Context, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return json.Marshal([]map[string]any{f.row})
}

func base64Tag(t *testing.T, data []byte) string {
	t.Helper()
	return "base64:" + base64.StdEncoding.EncodeToString(data)
}

// decodableGradientPNG loads the package's own golden-vector fixture, the
// smallest real decodable image already checked into the repo.
func decodableGradientPNG(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../hashing/testdata/gradient.png")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// TestExtractPreviewJPEGFallbackChainOrderAndFirstDecodableWins is the
// load-bearing test for the ported call sequence: PreviewImage absent
// (skipped), JpgFromRaw present but undecodable garbage (skipped),
// ThumbnailImage a real decodable image (taken) -- all from a single
// canned JSON row, proving the client-side order/fallback logic without
// needing three separate exiftool invocations.
func TestExtractPreviewJPEGFallbackChainOrderAndFirstDecodableWins(t *testing.T) {
	fake := &fakeExecutor{row: map[string]any{
		// PreviewImage: absent entirely.
		"JpgFromRaw":     base64Tag(t, []byte("not a real image")),
		"ThumbnailImage": base64Tag(t, decodableGradientPNG(t)),
	}}

	got, err := extractPreviewJPEG(context.Background(), fake, "notanimage.arw")
	if err != nil {
		t.Fatalf("extractPreviewJPEG: %v", err)
	}
	if got == nil {
		t.Fatal("extractPreviewJPEG returned nil, want the ThumbnailImage bytes")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("exiftool invocation count = %d, want exactly 1 (single combined request)", len(fake.calls))
	}

	img, _, err := image.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("decode returned preview: %v", err)
	}
	if img.Bounds().Empty() {
		t.Error("decoded preview image has an empty bounds rect")
	}
}

// TestExtractPreviewJPEGAllTagsExhausted proves that when every tag comes
// up empty, extractPreviewJPEG returns (nil, nil), not an error -- the
// normal case for a RAW with no embedded preview at all.
func TestExtractPreviewJPEGAllTagsExhausted(t *testing.T) {
	fake := &fakeExecutor{row: map[string]any{}}

	got, err := extractPreviewJPEG(context.Background(), fake, "notanimage.arw")
	if err != nil {
		t.Fatalf("extractPreviewJPEG: %v", err)
	}
	if got != nil {
		t.Errorf("extractPreviewJPEG with every tag empty = %v, want nil", got)
	}
}

// TestExtractPreviewJPEGRequestFailure proves a failed Execute call (e.g.
// the pool's own fallback fork also failing) is folded into (nil, nil),
// matching the "no usable preview" contract rather than surfacing as an
// error up through Extract.
func TestExtractPreviewJPEGRequestFailure(t *testing.T) {
	fake := &fakeExecutor{returnErr: errors.New("boom")}

	got, err := extractPreviewJPEG(context.Background(), fake, "notanimage.arw")
	if err != nil {
		t.Fatalf("extractPreviewJPEG: %v", err)
	}
	if got != nil {
		t.Errorf("extractPreviewJPEG on request failure = %v, want nil", got)
	}
}

// TestExtractIntegrationWithRealExiftool exercises the full Extract path
// (decodeFileAndHash fails, extractPreviewJPEG runs) against a real
// exiftool -stay_open pooled process and a real embedded thumbnail,
// proving the base64 JSON parsing and the pool wiring actually work end to
// end, not just against fakeExecutor.
func TestExtractIntegrationWithRealExiftool(t *testing.T) {
	exiftoolPath, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not found on PATH -- skipping real-binary phash test")
	}

	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.jpg")
	thumbPath := filepath.Join(dir, "thumb.jpg")
	if err := writeTinyJPEG(basePath, 4); err != nil {
		t.Fatalf("write base fixture: %v", err)
	}
	if err := writeTinyJPEG(thumbPath, 2); err != nil {
		t.Fatalf("write thumb fixture: %v", err)
	}
	cmd := exec.Command(exiftoolPath, "-overwrite_original", "-ThumbnailImage<="+thumbPath, "--", basePath) //nolint:gosec // test fixture paths under t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("exiftool write ThumbnailImage: %v\n%s", err, out)
	}

	// basePath itself IS a valid, directly-decodable JPEG, which would
	// short-circuit before ever reaching exiftool -- rename it to a RAW-
	// looking extension so decodeFileAndHash's image.Decode fails and
	// Extract falls through to the exiftool preview chain, the path this
	// test actually means to exercise.
	rawPath := filepath.Join(dir, "base.arw")
	if err := os.Rename(basePath, rawPath); err != nil {
		t.Fatal(err)
	}

	pool := exiftool.NewPool(exiftoolPath)
	defer pool.Close()

	got, err := Extract(context.Background(), pool, rawPath)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got == nil {
		t.Fatal("Extract returned nil, want a hash from the embedded ThumbnailImage")
	}
}

func writeTinyJPEG(path string, size int) error {
	f, err := os.Create(path) //nolint:gosec // test fixture path under t.TempDir()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), B: 128, A: 255})
		}
	}
	return jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
}
