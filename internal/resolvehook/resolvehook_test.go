package resolvehook

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const testFileName = "branchdam_render_hook.py"

func TestCandidateDirsWindows(t *testing.T) {
	dirs := CandidateDirs("windows", "/home/alice", `C:\Users\alice\AppData\Roaming`)
	if len(dirs) != 1 {
		t.Fatalf("got %d dirs, want 1: %v", len(dirs), dirs)
	}
	want := filepath.Join(`C:\Users\alice\AppData\Roaming`, "Blackmagic Design", "DaVinci Resolve", "Support", "Fusion", "Scripts", "Utility")
	if dirs[0] != want {
		t.Errorf("got %q, want %q", dirs[0], want)
	}
}

func TestCandidateDirsWindowsEmptyAppData(t *testing.T) {
	if dirs := CandidateDirs("windows", "/home/alice", ""); dirs != nil {
		t.Errorf("expected nil for empty appData, got %v", dirs)
	}
}

// TestCandidateDirsDarwinPerUserFirst pins the load-bearing correction to
// hooks/resolve/README.md: the per-user path must sort BEFORE the
// system-wide one, since an unprivileged tray can install to the former
// but not the latter (EACCES).
func TestCandidateDirsDarwinPerUserFirst(t *testing.T) {
	dirs := CandidateDirs("darwin", "/Users/alice", "")
	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}
	wantPerUser := filepath.Join("/Users/alice", "Library", "Application Support", "Blackmagic Design", "DaVinci Resolve", "Fusion", "Scripts", "Utility")
	wantSystem := "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility"
	if dirs[0] != wantPerUser {
		t.Errorf("dirs[0] = %q, want the per-user path %q first", dirs[0], wantPerUser)
	}
	if dirs[1] != wantSystem {
		t.Errorf("dirs[1] = %q, want the system-wide path %q second", dirs[1], wantSystem)
	}
}

func TestCandidateDirsDarwinEmptyHome(t *testing.T) {
	dirs := CandidateDirs("darwin", "", "")
	if len(dirs) != 1 {
		t.Fatalf("got %d dirs, want 1 (system-wide only): %v", len(dirs), dirs)
	}
}

func TestCandidateDirsLinuxDetectOnly(t *testing.T) {
	dirs := CandidateDirs("linux", "/home/alice", "")
	if len(dirs) != 1 || dirs[0] != "/opt/resolve/Fusion/Scripts/Utility" {
		t.Errorf("got %v", dirs)
	}
}

func TestDetectEmptyCandidateListYieldsEmptyDir(t *testing.T) {
	st := Detect(nil, testFileName, "irrelevant")
	if st.Dir != "" || st.Installed {
		t.Errorf("got %+v, want a zero-value HookState for an empty candidate list", st)
	}
}

func TestDetectNoCandidateDirectoryExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	st := Detect([]string{dir}, testFileName, "irrelevant")
	if st.Dir != "" || st.Installed {
		t.Errorf("got %+v, want Dir==\"\" when no candidate directory exists on disk", st)
	}
}

func TestDetectNotInstalledButDirectoryExists(t *testing.T) {
	dir := t.TempDir()
	st := Detect([]string{dir}, testFileName, "irrelevant")
	if st.Dir != dir {
		t.Errorf("Dir = %q, want %q", st.Dir, dir)
	}
	if st.Installed {
		t.Error("expected Installed=false -- the directory exists but the file doesn't")
	}
}

func TestFullInstallDetectMatrix(t *testing.T) {
	dir := t.TempDir()
	source := []byte("#!/usr/bin/env python3\nprint('hello')\n")
	wantSHA := sha256Hex(source)

	// 1. not-installed.
	st := Detect([]string{dir}, testFileName, wantSHA)
	if st.Installed {
		t.Fatalf("expected not-installed before Install, got %+v", st)
	}

	// 2. Install -> installed-and-up-to-date.
	installed, err := Install(dir, testFileName, source)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !installed.Installed || !installed.UpToDate {
		t.Errorf("Install's own return: got %+v, want Installed=true UpToDate=true", installed)
	}
	if installed.Path != filepath.Join(dir, testFileName) {
		t.Errorf("Path = %q", installed.Path)
	}

	st = Detect([]string{dir}, testFileName, wantSHA)
	if !st.Installed || !st.UpToDate {
		t.Fatalf("Detect after Install: got %+v, want Installed=true UpToDate=true", st)
	}

	// 3. mutate one byte -> installed but out of date/modified.
	mutated := append([]byte(nil), source...)
	mutated[0] = 'X'
	if err := os.WriteFile(filepath.Join(dir, testFileName), mutated, 0o644); err != nil {
		t.Fatal(err)
	}
	st = Detect([]string{dir}, testFileName, wantSHA)
	if !st.Installed || st.UpToDate {
		t.Fatalf("Detect after mutation: got %+v, want Installed=true UpToDate=false", st)
	}

	// 4. delete -> not-installed again.
	if err := os.Remove(filepath.Join(dir, testFileName)); err != nil {
		t.Fatal(err)
	}
	st = Detect([]string{dir}, testFileName, wantSHA)
	if st.Installed {
		t.Fatalf("Detect after delete: got %+v, want Installed=false", st)
	}
	if st.Dir != dir {
		t.Errorf("Dir = %q, want %q (the directory itself still exists)", st.Dir, dir)
	}
}

func TestInstallCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "Scripts", "Utility")
	source := []byte("print('x')\n")

	st, err := Install(dir, testFileName, source)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !st.Installed {
		t.Errorf("got %+v", st)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected Install to create the missing directory: %v", err)
	}
}

func TestInstallFileMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir, testFileName, []byte("x")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, testFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("got mode %v, want 0644", info.Mode().Perm())
	}
}

func TestInstallOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(dir, testFileName, []byte("old content")); err != nil {
		t.Fatal(err)
	}
	newContent := []byte("new content")
	if _, err := Install(dir, testFileName, newContent); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, testFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("got %q, want %q", got, newContent)
	}
	// No leftover .tmp-* file after a successful install.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected exactly 1 entry in %s after install, got %d: %v", dir, len(entries), entries)
	}
}
