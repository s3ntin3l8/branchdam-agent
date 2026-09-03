package tray

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

func TestHandleImportFolderSuccess(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{
		Files: []ingest.FileResult{{SourcePath: "/media/external/folder/test.jpg"}},
	}}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	var notified bool
	notify := func(_ context.Context, _ string) {
		notified = true
	}
	pickDir := func(_ context.Context) (string, error) {
		return "/media/external/folder", nil
	}

	if !handleImportFolder(context.Background(), r, pickDir, notify) {
		t.Fatal("expected handleImportFolder to return true on success")
	}
	if notified {
		t.Error("expected notification not to be called on success")
	}
	if len(fi.calls) != 1 || fi.calls[0] != "/media/external/folder" {
		t.Errorf("expected IngestCard called once with /media/external/folder, got %v", fi.calls)
	}
}

func TestHandleImportFolderAlreadyWatched(t *testing.T) {
	fi := &fakeIngester{}
	watchDir := filepath.Join("media", "card")
	r := NewRunner(fi, []string{watchDir}, "/scratch")

	var gotMsg string
	notify := func(_ context.Context, msg string) {
		gotMsg = msg
	}
	// Test with trailing slash / redundant separator to ensure normalization
	pickDir := func(_ context.Context) (string, error) {
		return watchDir + string(filepath.Separator), nil
	}

	if handleImportFolder(context.Background(), r, pickDir, notify) {
		t.Fatal("expected handleImportFolder to return false when path is already watched")
	}
	if gotMsg != "Already ingesting this path" {
		t.Errorf("got notification message %q, want %q", gotMsg, "Already ingesting this path")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderCanceled(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	var notified bool
	notify := func(_ context.Context, _ string) {
		notified = true
	}
	pickDir := func(_ context.Context) (string, error) {
		return "", errors.New("canceled")
	}

	if handleImportFolder(context.Background(), r, pickDir, notify) {
		t.Fatal("expected handleImportFolder to return false when picker canceled")
	}
	if notified {
		t.Error("expected notification not to be called on picker cancel")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderEmptyPath(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	pickDir := func(_ context.Context) (string, error) {
		return "", nil
	}

	if handleImportFolder(context.Background(), r, pickDir, nil) {
		t.Fatal("expected handleImportFolder to return false on empty path")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderNilSafeguards(t *testing.T) {
	if handleImportFolder(context.Background(), nil, nil, nil) {
		t.Error("expected false for nil runner/pickDir")
	}
}

func TestSamePathOS(t *testing.T) {
	cases := []struct {
		a, b string
		goos string
		want bool
	}{
		{"/media/card", "/media/card", "linux", true},
		{"/media/card", "/media/card/", "linux", true},
		{"/media/card", "/media/card/.", "linux", true},
		{"/media/card", "/media/CARD", "linux", false},
		{"/media/card", "/media/CARD", "windows", true},
		{"/media/card", "/media/CARD", "darwin", true},
		{"C:\\Volumes\\Card", "c:\\volumes\\card", "windows", true},
		{"/Volumes/Card", "/volumes/card", "darwin", true},
		{"/media/card1", "/media/card2", "windows", false},
		{"/media/card1", "/media/card2", "darwin", false},
	}

	for _, tc := range cases {
		got := samePathOS(tc.a, tc.b, tc.goos)
		if got != tc.want {
			t.Errorf("samePathOS(%q, %q, %q) = %v, want %v", tc.a, tc.b, tc.goos, got, tc.want)
		}
	}
}

// TestSamePathSymlinkResolution pins the symlink-resolution half of
// samePath: picking the same physical directory via a different path
// (the canonical macOS example is /var/folders/X ↔
// /private/var/folders/X, the canonical Linux example is /proc/<pid>/root
// ↔ /) must still register as already-watched, so the operator sees the
// "Already ingesting this path" toast instead of starting a redundant
// concurrent ingest of the same physical directory.
//
// Runs a real symlink on the host's temp dir (Linux CI is fine -- /tmp
// is a real path on every supported build host) rather than mocking
// EvalSymlinks: the bug is in the integration with os.Stat/os.EvalSymlinks,
// not in a hypothetical pure function, so a real fs is the right test.
func TestSamePathSymlinkResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on most Windows CI")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir real: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	if !samePath(target, link) {
		t.Errorf("samePath(%q, %q) = false, want true (should resolve %q through the symlink)", target, link, link)
	}
}

// TestSamePathMissingPathStillComparesLexically guards against a regression
// where EvalSymlinks could short-circuit samePath's lexical comparison
// when one side has been deleted between watcher setup and pick: a card
// root the operator has just ejected must still match any picked path
// whose lexical form is identical, so the user does not see a confusing
// notification about a watch entry we can no longer resolve.
func TestSamePathMissingPathStillComparesLexically(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "card-ejected")
	present := filepath.Join(dir, "card-present")
	if err := os.Mkdir(present, 0o755); err != nil {
		t.Fatalf("Mkdir present: %v", err)
	}
	if samePath(missing, present) {
		t.Errorf("samePath(%q, %q) = true, want false (unrelated paths)", missing, present)
	}
	if !samePath(missing, missing) {
		t.Errorf("samePath(%q, %q) = false, want true (identical paths)", missing, missing)
	}
}
