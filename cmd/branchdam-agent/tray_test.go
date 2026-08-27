package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunTrayUnsupportedOnLinux exercises the full wiring path (config
// load, branchdam.New, ingest.Engine, tray.NewRunner, the status server
// starting) against a fixture config, on whatever platform `go test` runs
// on. On Linux -- the only platform CI actually tests, per
// .github/workflows/ci-cd.yml -- tray.Run returns tray.ErrUnsupported
// immediately, so this also proves runTrayCmd doesn't hang or leak the
// status server's goroutine waiting for it. selfUpdate.enabled is
// explicitly false: selfUpdate.enabled now defaults to true (see
// internal/config.defaultConfig), and newSelfUpdateAgent's Run goroutine
// is fire-and-forget (joined via ctx cancellation, not a WaitGroup) -- a
// fixture that left it enabled would make this unit test perform a real
// network call to GitHub on every run.
func TestRunTrayUnsupportedOnLinux(t *testing.T) {
	dir := t.TempDir()
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")

	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"http://127.0.0.1:1\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + archiveRoot + "\"\n" +
		"  localEditRoot: \"" + localRoot + "\"\n" +
		"tray:\n" +
		"  statusAddr: \"127.0.0.1:0\"\n" +
		"selfUpdate:\n" +
		"  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"tray", "-config", cfgPath})
	if got != 1 {
		t.Errorf("run([tray]) = %d, want 1 (tray.ErrUnsupported on this platform)", got)
	}
}

func TestRunTrayMissingAPIKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("agentId: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Errorf("run([tray]) with empty apiKey = %d, want 1", got)
	}
}

func TestRunTrayMissingIngestRoots(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "server:\n  apiKey: \"0123456789abcdef0123456789abcdef\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Errorf("run([tray]) with no ingest roots = %d, want 1", got)
	}
}

func TestRunTrayMissingConfigFile(t *testing.T) {
	if got := run([]string{"tray", "-config", "/nonexistent/config.yaml"}); got != 1 {
		t.Errorf("run([tray]) with missing config = %d, want 1", got)
	}
}
