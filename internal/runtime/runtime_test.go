package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPathDelegatesToPathForGOOS(t *testing.T) {
	// Path() is a one-liner over pathForGOOS(runtime.GOOS); the
	// test pins the indirection so a future refactor (e.g.
	// adding a config-override hook) doesn't silently drop the
	// GOOS argument.
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want, err := pathForGOOS(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Path() = %q, want %q (must match pathForGOOS(runtime.GOOS))", got, want)
	}
	if filepath.Base(got) != "runtime.json" {
		t.Errorf("Path() = %q, want filename to be runtime.json", got)
	}
}

func TestPathForGOOSWindows(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\alice\AppData\Local`)
	got, err := pathForGOOS("windows")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(`C:\Users\alice\AppData\Local`, "branchDAM", "runtime.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPathForGOOSWindowsMissingEnv(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	if _, err := pathForGOOS("windows"); err == nil {
		t.Error("expected an error when LOCALAPPDATA is unset")
	}
}

func TestPathForGOOSDarwin(t *testing.T) {
	t.Setenv("HOME", "/Users/alice")
	got, err := pathForGOOS("darwin")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/Users/alice", "Library", "Application Support", "branchDAM", "runtime.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPathForGOOSLinuxWithXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/home/alice/.state")
	got, err := pathForGOOS("linux")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/alice/.state", "branchdam-agent", "runtime.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPathForGOOSLinuxFallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/alice")
	got, err := pathForGOOS("linux")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/alice", ".local", "state", "branchdam-agent", "runtime.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPathForGOOSHomeResolutionFails(t *testing.T) {
	// On Linux, with XDG_STATE_HOME unset and HOME pointing at
	// a path that os.UserHomeDir can't resolve (HOME=""), the
	// fall-back branch must return the UserHomeDir error
	// verbatim. The same error path is reachable on darwin
	// (whose home is always from UserHomeDir) and on any
	// platform with a broken HOME -- a test pinned here covers
	// both branches through a single setup.
	if runtime.GOOS == "windows" {
		t.Skip("windows uses %LOCALAPPDATA%, not os.UserHomeDir")
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if _, err := pathForGOOS("linux"); err == nil {
		t.Error("expected an error from pathForGOOS(linux) when HOME is empty")
	}
	if _, err := pathForGOOS("darwin"); err == nil {
		t.Error("expected an error from pathForGOOS(darwin) when HOME is empty")
	}
}

func TestPathForGOOSUnsupported(t *testing.T) {
	if _, err := pathForGOOS("plan9"); err == nil {
		t.Error("expected an error for an unsupported GOOS")
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	if err := Save(path, State{LastHandshakeAt: when}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastHandshakeAt.Equal(when) {
		t.Errorf("LastHandshakeAt = %v, want %v", got.LastHandshakeAt, when)
	}
}

func TestLoadMissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.json")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if !got.LastHandshakeAt.IsZero() {
		t.Errorf("LastHandshakeAt = %v, want zero time.Time{}", got.LastHandshakeAt)
	}
}

func TestLoadCorruptJSONReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte("not json {"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("expected nil error for corrupt JSON (non-fatal), got %v", err)
	}
	if !got.LastHandshakeAt.IsZero() {
		t.Errorf("LastHandshakeAt = %v, want zero time.Time{}", got.LastHandshakeAt)
	}
}

func TestLoadEmptyFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("expected nil error for empty file, got %v", err)
	}
	if !got.LastHandshakeAt.IsZero() {
		t.Errorf("LastHandshakeAt = %v, want zero time.Time{}", got.LastHandshakeAt)
	}
}

func TestLoadReturnsWrappedErrorForNonENOENT(t *testing.T) {
	// A non-ENOENT I/O error (the file exists but can't be read
	// for some other reason) must be returned to the caller
	// wrapped with the runtime: prefix, not silently swallowed.
	// The cmd wiring relies on this to distinguish "the file
	// doesn't exist" (no-op) from "the file exists but is
	// unreadable" (the "you had a cross-restart signal and
	// it's now lost" case that triggers the ERROR log + save
	// callback skip).
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based tests are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses POSIX permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(`{"lastHandshakeAt":"2026-09-03T12:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Strip the read bit so the test user can't read the file
	// back. The chmod 0o000 makes the file unreadable but
	// still statable -- os.ReadFile fails with EACCES, which
	// is a non-ENOENT error and must propagate.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(path, 0o600) }() // for t.TempDir cleanup
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected Load to return a non-nil error when the file is unreadable")
	}
	if !strings.Contains(err.Error(), "runtime:") {
		t.Errorf("Load error = %v, want one wrapped with the runtime: prefix", err)
	}
}

func TestSaveIsAtomicNoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := Save(path, State{LastHandshakeAt: when}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file %q left behind after Save", e.Name())
		}
	}
}

func TestSaveOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := Save(path, State{LastHandshakeAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, State{LastHandshakeAt: fresh}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastHandshakeAt.Equal(fresh) {
		t.Errorf("LastHandshakeAt = %v, want %v", got.LastHandshakeAt, fresh)
	}
}

func TestSaveCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "runtime.json")
	if err := Save(path, State{LastHandshakeAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Save should have created the file at %q: %v", path, err)
	}
}

func TestSaveFileModeIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := Save(path, State{LastHandshakeAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestSaveParentDirectoryModeIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows")
	}
	dir := t.TempDir()
	// Use a non-existent subdirectory so MkdirAll has to create it
	// (and the chmod-after has to actually retighten any pre-existing
	// 0o755 path -- here the tempdir is 0o700, but the nested dir is
	// freshly created at 0o700 and the chmod is a no-op-on-success).
	nested := filepath.Join(dir, "nested", "state", "dir")
	path := filepath.Join(nested, "runtime.json")
	if err := Save(path, State{LastHandshakeAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700 (world-readable dir leaks the existence of the runtime state file)", perm)
	}
}

func TestSaveTightensPreExistingPermissiveDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod is a no-op on Windows")
	}
	dir := t.TempDir()
	permissive := filepath.Join(dir, "permissive")
	if err := os.Mkdir(permissive, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(permissive, "runtime.json")
	if err := Save(path, State{LastHandshakeAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(permissive)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("after Save over a pre-existing 0755 dir: mode = %o, want 0700", perm)
	}
}

// TestSaveWritesValidJSON verifies the on-disk format is
// parseable by a different decoder (the cmd package's
// runtimeState.Load, which uses encoding/json like the
// rest of the repo). The integration is exercised by
// TestWireRuntimeStatePersistenceRealWiringReachesTheDisk
// in cmd/branchdam-agent.
func TestSaveWritesValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	when := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := Save(path, State{LastHandshakeAt: when}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Errorf("Save wrote invalid JSON: %q", b)
	}
}

// fakeWriteCloser is a minimal writeCloseRenamer for testing
// Save's error paths. The underlying file is a real *os.File
// (so os.Rename can find it on disk); Write/Close are
// overridden to return the configured error. This lets the
// test exercise Save's error paths without the chicken-and-
// egg problem of "the rename fails because the fake temp
// file doesn't actually exist."
type fakeWriteCloser struct {
	*os.File
	writeErr error
	closeErr error
}

func (f *fakeWriteCloser) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(p)
}

func (f *fakeWriteCloser) Close() error {
	if f.closeErr != nil {
		return f.closeErr
	}
	return f.File.Close()
}

// newFakeTempInDir creates a real on-disk temp file inside dir
// and returns it wrapped as a fakeWriteCloser. The file is
// renamed-or-cleaned-up by Save; tests don't need to track
// its lifecycle beyond what Save already does.
func newFakeTempInDir(t *testing.T, dir string) *fakeWriteCloser {
	t.Helper()
	f, err := os.CreateTemp(dir, "tmp.fake.*")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeWriteCloser{File: f}
}

// TestSaveCreateTempFailure covers the CreateTemp error path
// in saveWithOps. Without ops injection, this branch is only
// reachable when the parent dir is unwritable -- a state
// that Save's dir-mode-tightening chmod actively defends
// against on every call, so the branch was unreachable in
// tests prior to the ops injection.
func TestSaveCreateTempFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	realMkdir := os.MkdirAll
	realChmod := os.Chmod
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll: realMkdir,
		Chmod:    realChmod,
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) {
			return nil, errors.New("simulated ENOSPC")
		},
		Rename:   os.Rename,
		fsyncDir: fsyncDir,
	})
	if err == nil {
		t.Fatal("expected Save to fail when CreateTemp fails")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Errorf("error = %v, want one wrapped with 'create temp file'", err)
	}
}

// TestSaveWriteFailure covers the tmp.Write error path.
func TestSaveWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmp := newFakeTempInDir(t, dir)
	tmp.writeErr = errors.New("disk full mid-write")
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      os.Chmod,
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) { return tmp, nil },
		Rename:     os.Rename,
		fsyncDir:   fsyncDir,
	})
	if err == nil {
		t.Fatal("expected Save to fail when tmp.Write fails")
	}
	if !strings.Contains(err.Error(), "write temp file") {
		t.Errorf("error = %v, want one wrapped with 'write temp file'", err)
	}
}

// TestSaveCloseFailure covers the tmp.Close error path.
func TestSaveCloseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmp := newFakeTempInDir(t, dir)
	tmp.closeErr = errors.New("simulated EIO on close")
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      os.Chmod,
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) { return tmp, nil },
		Rename:     os.Rename,
		fsyncDir:   fsyncDir,
	})
	if err == nil {
		t.Fatal("expected Save to fail when tmp.Close fails")
	}
	if !strings.Contains(err.Error(), "close temp file") {
		t.Errorf("error = %v, want one wrapped with 'close temp file'", err)
	}
}

// TestSaveRenameFailure covers the os.Rename error path.
func TestSaveRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmp := newFakeTempInDir(t, dir)
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      os.Chmod,
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) { return tmp, nil },
		Rename:     func(_, _ string) error { return errors.New("simulated EXDEV") },
		fsyncDir:   fsyncDir,
	})
	if err == nil {
		t.Fatal("expected Save to fail when Rename fails")
	}
	if !strings.Contains(err.Error(), "rename temp file") {
		t.Errorf("error = %v, want one wrapped with 'rename temp file'", err)
	}
}

// TestSaveChmodFailureIsNonFatal covers the chmod-temp-to-0o600
// error path. Unlike CreateTemp/Write/Close/Rename failures,
// a chmod failure is best-effort: it logs at WARN and
// continues. The test pins the "non-fatal" property of this
// branch -- the Save call returns nil and the file at
// `path` is still written correctly via the rename.
func TestSaveChmodFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmp := newFakeTempInDir(t, dir)
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      func(string, os.FileMode) error { return errors.New("chmod not supported on this FS") },
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) { return tmp, nil },
		Rename:     os.Rename,
		fsyncDir:   fsyncDir,
	})
	if err != nil {
		t.Fatalf("expected Save to succeed when only the chmod-of-temp fails (the chmod is best-effort), got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %q after the chmod-failed Save, got %v", path, err)
	}
}

// TestSaveFsyncDirFailureIsNonFatal covers the fsyncDir error
// path. Like chmod, fsyncDir failure is best-effort: the
// Save returns nil and the file at `path` is written. The
// caller logs the failure as WARN -- in this case the agent
// accepts that a power loss could revert to the prior
// runtime.json, which the in-memory carry-forward defends
// against in-session.
func TestSaveFsyncDirFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	tmp := newFakeTempInDir(t, dir)
	err := saveWithOps(path, State{LastHandshakeAt: time.Now()}, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      os.Chmod,
		CreateTemp: func(_, _ string) (writeCloseRenamer, error) { return tmp, nil },
		Rename:     os.Rename,
		fsyncDir:   func(string) error { return errors.New("simulated ENOTSUP on dir fsync") },
	})
	if err != nil {
		t.Fatalf("expected Save to succeed when only the dir fsync fails (best-effort), got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %q after the fsync-failed Save, got %v", path, err)
	}
}

func TestSaveReturnsErrorForParentThatIsAFile(t *testing.T) {
	// The only path guarantee we can make on any POSIX system
	// is "the parent is a regular file" -- CreateTemp will fail
	// with ENOTDIR. A 0o555 dir would also fail, but Save's
	// dir-mode-tightening chmod (which exists to defend a
	// pre-existing 0o755 dir from a 5s drain cadence) retightens
	// 0o555 to 0o700 on the next save, defeating a 0o555-based
	// test. An ENOTDIR failure, by contrast, is non-recoverable:
	// no chmod can turn a regular file into a directory.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "i-am-a-file")
	if err := os.WriteFile(notADir, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notADir, "runtime.json")
	if err := Save(path, State{LastHandshakeAt: time.Now()}); err == nil {
		t.Fatal("expected Save to fail when parent is a regular file, not a directory")
	}
}

func TestStateJSONRoundTripExplicit(t *testing.T) {
	// Pins the JSON shape: LastHandshakeAt as a non-nil-but-zero field
	// must round-trip as "" in JSON (per `json:"lastHandshakeAt,omitempty"`)
	// and back to the zero time.Time -- not a null that's read as a
	// sentinel for "missing." The status page template's
	// `{{ if not .Status.LastHandshakeAt.IsZero }}` guard is the
	// "never" sentinel; the JSON layer must respect that.
	st := State{}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("zero State marshals to %q, want %q (omitempty must drop the zero time)", b, "{}")
	}
	var back State
	if err := json.Unmarshal([]byte("{}"), &back); err != nil {
		t.Fatal(err)
	}
	if !back.LastHandshakeAt.IsZero() {
		t.Errorf("unmarshaled State.LastHandshakeAt = %v, want zero time.Time{}", back.LastHandshakeAt)
	}
}
