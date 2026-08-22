package phash

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// adversarialPaths mirrors branchDAM's own probe/argv_test.go fixture list --
// paths crafted to look like exiftool flags, proving "--" actually stops
// them from being parsed as such.
var adversarialPaths = []string{
	"/normal/path/DSC001.ARW",
	"-overwrite_original",
	"-TAG=value.jpg",
	"--exec=rm -rf /",
	"-b",
	"",
}

var tagAssignmentRe = regexp.MustCompile(`^-[A-Za-z0-9:_-]+=`)

func checkArgvShape(t *testing.T, fn func(string) []string) {
	t.Helper()
	for _, path := range adversarialPaths {
		args := fn(path)
		sawSep := false
		for _, a := range args {
			if a == "--" {
				sawSep = true
				continue
			}
			if sawSep {
				continue
			}
			if tagAssignmentRe.MatchString(a) {
				t.Errorf("args(%q) contains a tag assignment before --: %v", path, args)
			}
		}
		if !sawSep {
			t.Errorf("args(%q) has no -- separator: %v", path, args)
		}
		if args[len(args)-1] != path {
			t.Errorf("args(%q) does not end with the path: %v", path, args)
		}
	}
}

func TestPreviewImageArgsShape(t *testing.T)   { checkArgvShape(t, previewImageArgs) }
func TestJpgFromRawArgsShape(t *testing.T)     { checkArgvShape(t, jpgFromRawArgs) }
func TestThumbnailImageArgsShape(t *testing.T) { checkArgvShape(t, thumbnailImageArgs) }

func TestPreviewImageArgsExactFlags(t *testing.T) {
	if got, want := previewImageArgs("/x.arw"), []string{"-b", "-PreviewImage", "--", "/x.arw"}; !equalArgs(got, want) {
		t.Errorf("previewImageArgs = %v, want %v", got, want)
	}
	if got, want := jpgFromRawArgs("/x.arw"), []string{"-b", "-JpgFromRaw", "--", "/x.arw"}; !equalArgs(got, want) {
		t.Errorf("jpgFromRawArgs = %v, want %v", got, want)
	}
	if got, want := thumbnailImageArgs("/x.arw"), []string{"-b", "-ThumbnailImage", "--", "/x.arw"}; !equalArgs(got, want) {
		t.Errorf("thumbnailImageArgs = %v, want %v", got, want)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestExtractDirectDecodeSkipsExiftool proves the fast path never shells out
// at all: exiftoolPath points at a binary that does not exist, and Extract
// must still succeed (and match hashing's own golden vector) because
// gradient.png decodes directly.
func TestExtractDirectDecodeSkipsExiftool(t *testing.T) {
	got, err := Extract(context.Background(), "/nonexistent/exiftool-binary-that-must-never-run", "../hashing/testdata/gradient.png")
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

// TestExtractNoExiftoolNoDirectDecode proves the (nil, nil) "nothing usable"
// contract: a file that fails direct decode, with no exiftool available at
// all (empty path), must return (nil, nil), not an error.
func TestExtractNoExiftoolNoDirectDecode(t *testing.T) {
	dir := t.TempDir()
	notAnImage := filepath.Join(dir, "notanimage.arw")
	if err := os.WriteFile(notAnImage, []byte("this is not image data"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Extract(context.Background(), "", notAnImage)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("Extract with no exiftool and no direct decode = %v, want nil", got)
	}
}

// fakeExiftool writes a minimal shell script standing in for exiftool: it
// inspects the tag flag it was invoked with (argv[2], since argv[1] is
// always "-b") and, per behaviors keyed by that tag, either prints nothing
// (simulating an absent tag), prints undecodable garbage (simulating a
// corrupt/empty embedded preview), or dumps a real decodable image fixture.
// It also appends the tag it was called with, one per line, to a log file
// so the test can assert both WHICH tags were tried and in WHAT ORDER --
// the property that actually matters here, since real exiftool binary
// behavior against a real RAW file is out of scope for this repo (no RAW
// fixtures, and reproducing exiftool's RAW-parsing itself is explicitly not
// what's being ported -- see the package doc).
func fakeExiftool(t *testing.T, behaviors map[string]string, logPath string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake exiftool shell script requires a POSIX shell")
	}

	gradientPNG, err := os.ReadFile("../hashing/testdata/gradient.png")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "gradient.png")
	if err := os.WriteFile(fixturePath, gradientPNG, 0o644); err != nil {
		t.Fatal(err)
	}

	script := "#!/bin/sh\n" +
		"tag=\"$2\"\n" +
		"echo \"$tag\" >> " + shellQuote(logPath) + "\n" +
		"case \"$tag\" in\n"
	for tag, behavior := range behaviors {
		switch behavior {
		case "empty":
			script += "  " + tag + ") exit 0 ;;\n"
		case "garbage":
			script += "  " + tag + ") printf 'not a real image' ;;\n"
		case "decodable":
			script += "  " + tag + ") cat " + shellQuote(fixturePath) + " ;;\n"
		}
	}
	script += "esac\n"

	scriptPath := filepath.Join(dir, "fake-exiftool.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath
}

func shellQuote(s string) string {
	return "'" + s + "'"
}

// TestExtractFallbackChainOrderAndFirstDecodableWins is the load-bearing
// test for the ported call sequence: PreviewImage empty (skipped),
// JpgFromRaw present but undecodable garbage (skipped), ThumbnailImage a
// real decodable image (taken) -- and never tries anything past that.
func TestExtractFallbackChainOrderAndFirstDecodableWins(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	notAnImage := filepath.Join(dir, "notanimage.arw")
	if err := os.WriteFile(notAnImage, []byte("raw sensor data, not decodable"), 0o644); err != nil {
		t.Fatal(err)
	}

	exiftool := fakeExiftool(t, map[string]string{
		"-PreviewImage":   "empty",
		"-JpgFromRaw":     "garbage",
		"-ThumbnailImage": "decodable",
	}, logPath)

	got, err := Extract(context.Background(), exiftool, notAnImage)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got == nil {
		t.Fatal("Extract returned nil, want a hash from the ThumbnailImage fallback")
	}
	const want = -6161209626521968640
	if *got != want {
		t.Errorf("Extract via fallback = %d, want %d", *got, want)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	const wantLog = "-PreviewImage\n-JpgFromRaw\n-ThumbnailImage\n"
	if string(log) != wantLog {
		t.Errorf("exiftool invocation order = %q, want %q", log, wantLog)
	}
}

// TestExtractFallbackChainAllTagsExhausted proves that when every tag comes
// up empty, Extract returns (nil, nil), not an error -- the normal case for
// a RAW with no embedded preview at all.
func TestExtractFallbackChainAllTagsExhausted(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "invocations.log")
	notAnImage := filepath.Join(dir, "notanimage.arw")
	if err := os.WriteFile(notAnImage, []byte("raw sensor data, not decodable"), 0o644); err != nil {
		t.Fatal(err)
	}

	exiftool := fakeExiftool(t, map[string]string{
		"-PreviewImage":   "empty",
		"-JpgFromRaw":     "empty",
		"-ThumbnailImage": "empty",
	}, logPath)

	got, err := Extract(context.Background(), exiftool, notAnImage)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got != nil {
		t.Errorf("Extract with every tag empty = %v, want nil", got)
	}
}
