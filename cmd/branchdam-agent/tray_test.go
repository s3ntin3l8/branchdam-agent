package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// stubTrayDialog overrides trayDialogSetup for the duration of a test, so
// runTrayCmd's dialog-driven paths (the startup-error notification, the
// first-run setup wizard) never re-exec the actual `go test` binary as
// `dialog ...` -- see trayDialogSetup's own doc comment for why that would
// be a problem. run defaults to one that fails every call (matching
// "no display available," the common CI case) when nil; pass a custom run
// to drive the first-run wizard through specific answers.
//
// Also redirects every env var agentlog.Path consults (XDG_STATE_HOME,
// HOME, LOCALAPPDATA) into this test's own t.TempDir(), since runTrayCmd
// now calls the real agentlog.Setup() unconditionally -- without this, a
// bare `go test ./...` would write real files under the actual
// developer's or CI runner's home/state directory, on whatever OS branch
// runtime.GOOS actually resolves to at test time.
func stubTrayDialog(t *testing.T, run dialogRunner) {
	t.Helper()
	if run == nil {
		run = func(args ...string) (string, int, error) { return "", dialogExitFailed, nil }
	}
	orig := trayDialogSetup
	trayDialogSetup = func() (dialogRunner, string, error) {
		return run, "/fake/self/exe", nil
	}
	t.Cleanup(func() { trayDialogSetup = orig })

	logDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(logDir, "xdg-state"))
	t.Setenv("HOME", filepath.Join(logDir, "home"))
	t.Setenv("LOCALAPPDATA", filepath.Join(logDir, "localappdata"))
}

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
	stubTrayDialog(t, nil)

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
	stubTrayDialog(t, nil)

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
	stubTrayDialog(t, nil)

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

// TestRunTrayValidatePlaceholderIsFatal pins the same footgun preflight
// already guards against (PR1): an unset ${VAR} left as a literal
// placeholder in server.apiKey must fail the tray outright, not proceed to
// a confusing downstream auth failure.
func TestRunTrayValidatePlaceholderIsFatal(t *testing.T) {
	stubTrayDialog(t, nil)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "server:\n  apiKey: \"${TEST_UNSET_VAR_XYZ}\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Errorf("run([tray]) with an unexpanded placeholder = %d, want 1", got)
	}
}

// TestRunTrayMissingConfigFileBootstrapsThenFails exercises issue #30's
// first-run path end to end when no dialog backend is available (the
// common CI/headless case, via stubTrayDialog's default fail-every-call
// runner): a starter config must still land on disk even though the
// wizard itself can't complete, matching `init`'s "always leave something
// to hand-edit" guarantee.
func TestRunTrayMissingConfigFileBootstrapsThenFails(t *testing.T) {
	stubTrayDialog(t, nil)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Errorf("run([tray]) with missing config and no dialog backend = %d, want 1", got)
	}

	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("expected a starter config to be written to %s: %v", cfgPath, err)
	}
}

// TestRunTrayFirstRunBootstrapAppliesAnswers drives the setup wizard with a
// fake dialogRunner answering every prompt, then confirms those answers
// actually landed in the config before runTrayCmd went on to load it --
// proving the bootstrap-then-load ordering in runTrayCmd, not just
// bootstrapConfigInteractive in isolation (see bootstrap_test.go for that).
func TestRunTrayFirstRunBootstrapAppliesAnswers(t *testing.T) {
	answers := map[string]string{
		"server.baseUrl":       "https://branchdam.example.com",
		"server.apiKey":        "0123456789abcdef0123456789abcdef",
		"ingest.archiveRoot":   "/archive",
		"ingest.localEditRoot": "/edit",
	}
	stubTrayDialog(t, func(args ...string) (string, int, error) {
		var title string
		for i, a := range args {
			if a == "-title" && i+1 < len(args) {
				title = args[i+1]
			}
		}
		for _, p := range bootstrapPrompts {
			if p.title == title {
				return answers[p.key], dialogExitOK, nil
			}
		}
		// Anything else is runTrayCmd's own post-bootstrap startup-error
		// notification (once tray.Run hits tray.ErrUnsupported on this
		// platform, see TestRunTrayUnsupportedOnLinux) -- not a bootstrap
		// prompt this test cares about the content of.
		return "", dialogExitFailed, nil
	})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Exit 1 is still expected here: bootstrap succeeds and runTrayCmd goes
	// on to build a real Engine/Runner/status server, but tray.Run itself
	// hits tray.ErrUnsupported on Linux (see TestRunTrayUnsupportedOnLinux).
	// What this test actually pins is that bootstrap ran and its answers
	// were persisted -- checked below, not the final exit code.
	_ = run([]string{"tray", "-config", cfgPath})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load bootstrapped config: %v", err)
	}
	if cfg.Server.BaseURL != answers["server.baseUrl"] {
		t.Errorf("server.baseUrl = %q, want %q", cfg.Server.BaseURL, answers["server.baseUrl"])
	}
	if cfg.Ingest.ArchiveRoot != answers["ingest.archiveRoot"] {
		t.Errorf("ingest.archiveRoot = %q, want %q", cfg.Ingest.ArchiveRoot, answers["ingest.archiveRoot"])
	}
}
