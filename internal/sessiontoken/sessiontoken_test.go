package sessiontoken

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withXDGStateHome points agentlog.Path (and therefore this package) at a
// temp directory on non-Windows/darwin GOOS -- mirrors the seam
// internal/agentlog's own tests rely on (pathForGOOS's "other" branch).
func withXDGStateHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skipf("agentlog.Path on %s doesn't honor XDG_STATE_HOME", runtime.GOOS)
	}
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

func TestGenerateWritesFileBesideAgentLog(t *testing.T) {
	dir := withXDGStateHome(t)

	token, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(token) == 0 {
		t.Fatal("token is empty")
	}

	path := filepath.Join(dir, "branchdam-agent", FileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 0600", perm)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != token {
		t.Errorf("file contents = %q, want %q", got, token)
	}
}

func TestGenerateRetightensPreExistingLoosePermissions(t *testing.T) {
	dir := withXDGStateHome(t)

	path := filepath.Join(dir, "branchdam-agent", FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 0600 (os.WriteFile alone only applies mode on creation)", perm)
	}
}

func TestGenerateIsFreshEveryCall(t *testing.T) {
	withXDGStateHome(t)

	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("Generate returned the same token twice in a row")
	}
}

func TestReadReturnsWhatGenerateWrote(t *testing.T) {
	withXDGStateHome(t)

	token, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != token {
		t.Errorf("Read() = %q, want %q", got, token)
	}
}

func TestReadFailsWhenNoTokenHasBeenGenerated(t *testing.T) {
	withXDGStateHome(t)

	if _, err := Read(); err == nil {
		t.Error("Read succeeded with no token file present, want an error")
	}
}
