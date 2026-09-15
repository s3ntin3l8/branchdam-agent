package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withXDGStateHome points agentlog.Path (and therefore GenerateSessionToken)
// at a temp directory on non-Windows/darwin GOOS -- mirrors the seam
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

func TestGenerateSessionTokenWritesFileBesideAgentLog(t *testing.T) {
	dir := withXDGStateHome(t)

	token, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken: %v", err)
	}
	if len(token) == 0 {
		t.Fatal("token is empty")
	}

	path := filepath.Join(dir, "branchdam-agent", sessionTokenFileName)
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

func TestGenerateSessionTokenIsFreshEveryCall(t *testing.T) {
	withXDGStateHome(t)

	first, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("GenerateSessionToken returned the same token twice in a row")
	}
}
