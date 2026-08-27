package agentlog

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathForGOOSWindows(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\alice\AppData\Local`)
	got, err := pathForGOOS("windows")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(`C:\Users\alice\AppData\Local`, "branchDAM", "logs", "agent.log")
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
	want := filepath.Join("/Users/alice", "Library", "Logs", "branchDAM", "agent.log")
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
	want := filepath.Join("/home/alice/.state", "branchdam-agent", "agent.log")
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
	want := filepath.Join("/home/alice", ".local", "state", "branchdam-agent", "agent.log")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSetupWritesToFileAndStderrAndCreatesDir confirms Setup creates the
// parent directory, writes to the file it names, and installs itself as
// slog's process-wide default -- restored afterward so this test can't
// leak its handler into any other test in the package (or, more
// importantly, any other package's tests running in the same binary).
func TestSetupWritesToFileAndStderrAndCreatesDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))

	origDefault := slog.Default()
	defer slog.SetDefault(origDefault)

	path, closeFn, err := Setup()
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = closeFn() }()

	wantPath := filepath.Join(dir, "xdg-state", "branchdam-agent", "agent.log")
	if path != wantPath {
		t.Errorf("got path %q, want %q", path, wantPath)
	}

	slog.Info("hello from test", "k", "v")

	if err := closeFn(); err != nil {
		t.Fatalf("closeFn: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello from test") {
		t.Errorf("log file missing expected message, got: %s", data)
	}
}

func TestSetupFallsBackToStderrOnPathError(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")

	origDefault := slog.Default()
	defer slog.SetDefault(origDefault)

	// pathForGOOS("windows") always fails without LOCALAPPDATA -- Setup
	// itself always uses runtime.GOOS, so on a non-Windows test runner
	// this exercises the success path instead. Directly test the fallback
	// behavior Setup implements when Path() fails by calling the same
	// code Setup would on an error, via a controlled failure: an
	// unwritable directory achieves the same "can't open the log file"
	// branch on any OS.
	dir := t.TempDir()
	unwritable := filepath.Join(dir, "no-such-parent")
	if err := os.WriteFile(unwritable, []byte("occupied by a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", unwritable)

	path, closeFn, err := Setup()
	if err == nil {
		t.Fatal("expected an error when the log directory can't be created")
	}
	defer func() { _ = closeFn() }()
	if path == "" {
		t.Error("expected Setup to still report the path it tried to use")
	}

	// Default logger must still work (stderr fallback), not panic or be nil.
	slog.Info("still logging somewhere")
}

func TestRotateIfLargeRenamesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	big := make([]byte, maxSizeBytes+1)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}

	rotateIfLarge(path)

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated file %s.1 to exist: %v", path, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("expected original path to be gone after rotation")
	}
}

func TestRotateIfLargeLeavesSmallFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}

	rotateIfLarge(path)

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("did not expect rotation for a small file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected original path to still exist: %v", err)
	}
}

func TestSlogBridgePrintAndPrintf(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	origDefault := slog.Default()
	defer slog.SetDefault(origDefault)

	path, closeFn, err := Setup()
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer func() { _ = closeFn() }()

	bridge := SlogBridge{}
	bridge.Print("plain message", 1, 2)
	bridge.Printf("formatted %s %d", "message", 42)

	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "plain message") {
		t.Errorf("Print's message missing from log, got: %s", data)
	}
	if !strings.Contains(string(data), "formatted message 42") {
		t.Errorf("Printf's message missing from log, got: %s", data)
	}
}
