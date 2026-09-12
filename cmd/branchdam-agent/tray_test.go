package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
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
		run = func(_ context.Context, args ...string) (string, int, error) { return "", dialogExitFailed, nil }
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
		"pathMappings:\n" +
		"  - workstationPath: \"" + archiveRoot + "\"\n" +
		"    containerPath: \"/storage/archive\"\n" +
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

// TestRunTrayMissingPathMappings pins the gap Hermes review caught on this
// PR: ingest.archiveRoot/localEditRoot alone being non-empty isn't enough
// -- a tray with no pathMappings entry launches fine but fails the first
// real card with a confusing ErrNoPathMapping (internal/ingest/pathmap.go)
// deep inside ingest, not at startup. Must fail fast instead.
func TestRunTrayMissingPathMappings(t *testing.T) {
	stubTrayDialog(t, nil)

	dir := t.TempDir()
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "server:\n  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"ingest:\n  archiveRoot: \"" + archiveRoot + "\"\n  localEditRoot: \"" + localRoot + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Errorf("run([tray]) with no pathMappings = %d, want 1", got)
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

// TestRunTrayMissingConfigFileWritesStarterConfig exercises the first-run
// path: when no config file exists, a starter config must land on disk
// and the tray must start in "not configured" mode (exit code 0 on
// supported platforms, or ErrUnsupported on Linux CI).
func TestRunTrayMissingConfigFileWritesStarterConfig(t *testing.T) {
	stubTrayDialog(t, nil)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	got := run([]string{"tray", "-config", cfgPath})
	// On Linux CI, tray returns ErrUnsupported (exit 1). On Windows/macOS,
	// it starts in "not configured" mode (exit 0). Both are acceptable.
	if got != 0 && got != 1 {
		t.Errorf("run([tray]) with missing config = %d, want 0 or 1", got)
	}

	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("expected a starter config to be written to %s: %v", cfgPath, err)
	}
}

// TestRunTrayFirstRunWritesStarterConfig verifies that when no config file
// exists, a starter config is written with default/empty fields. The old
// bootstrap wizard (5 dialog prompts) has been replaced by an installer-driven
// flow: the installer writes a starter config, then the tray starts in
// "not configured" mode and the user configures through the Settings menu.
func TestRunTrayFirstRunWritesStarterConfig(t *testing.T) {
	stubTrayDialog(t, nil)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Exit 1 is expected here: on Linux, tray.Run hits ErrUnsupported.
	// What this test pins is that a starter config was written with
	// empty fields -- checked below, not the final exit code.
	_ = run([]string{"tray", "-config", cfgPath})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load starter config: %v", err)
	}
	if cfg.Server.BaseURL != "" {
		t.Errorf("server.baseUrl = %q, want empty (configured via tray Settings)", cfg.Server.BaseURL)
	}
	if cfg.Server.APIKey != "" {
		t.Errorf("server.apiKey = %q, want empty (configured via tray Settings)", cfg.Server.APIKey)
	}
	if cfg.Ingest.ArchiveRoot != "" {
		t.Errorf("ingest.archiveRoot = %q, want empty (configured via tray Settings)", cfg.Ingest.ArchiveRoot)
	}
	if len(cfg.PathMappings) != 0 {
		t.Errorf("pathMappings = %+v, want empty (configured via tray Settings)", cfg.PathMappings)
	}
}

func TestRunTraySyncsNamingTemplateAtStartup(t *testing.T) {
	stubTrayDialog(t, nil)

	var handshakeCalled int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/handshake" {
			atomic.AddInt32(&handshakeCalled, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"ok": true,
				"serverVersion": "0.12.0",
				"serverTimeUnix": 1756470000,
				"namingTemplate": "{yyyy}/{camera_model}/{original_name}"
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")

	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + archiveRoot + "\"\n" +
		"  localEditRoot: \"" + localRoot + "\"\n" +
		"  pathTemplate: \"{original_name}\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: \"" + archiveRoot + "\"\n" +
		"    containerPath: \"/storage/archive\"\n" +
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
	if atomic.LoadInt32(&handshakeCalled) != 1 {
		t.Errorf("handshakeCalled = %d, want 1", handshakeCalled)
	}
}

func TestRunTrayHandshakeUnreachableContinuesStartup(t *testing.T) {
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
		"  pathTemplate: \"{original_name}\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: \"" + archiveRoot + "\"\n" +
		"    containerPath: \"/storage/archive\"\n" +
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

func TestRunTrayMissingOfflineTier0ContainerRoot(t *testing.T) {
	var messages []string
	stubTrayDialog(t, func(_ context.Context, args ...string) (string, int, error) {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-message" {
				messages = append(messages, args[i+1])
			}
		}
		return "", dialogExitFailed, nil
	})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: test-agent\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + filepath.Join(dir, "archive") + "\"\n" +
		"  localEditRoot: \"" + filepath.Join(dir, "local") + "\"\n" +
		"  cardRoots: [\"/media/card\"]\n" +
		"offline:\n" +
		"  queueDbPath: \"" + filepath.Join(dir, "queue.db") + "\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: \"" + filepath.Join(dir, "archive") + "\"\n" +
		"    containerPath: /storage/archive\n" +
		"tray:\n" +
		"  statusAddr: \"127.0.0.1:0\"\n" +
		"selfUpdate:\n" +
		"  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := run([]string{"tray", "-config", cfgPath}); got != 1 {
		t.Fatalf("run([tray]) = %d, want 1", got)
	}
	found := false
	for _, message := range messages {
		if strings.Contains(message, "offline.tier0ContainerRoot must be set") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("startup dialog messages = %v, want missing tier0 container root", messages)
	}
}

func TestProbeArchiveLocal(t *testing.T) {
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if !probeArchive(context.Background(), archiveDir, nil, false) {
		t.Errorf("probeArchive(%q) = false, want true for existing directory", archiveDir)
	}

	missingDir := filepath.Join(dir, "nonexistent")
	if probeArchive(context.Background(), missingDir, nil, false) {
		t.Errorf("probeArchive(%q) = true, want false for missing directory", missingDir)
	}

	if probeArchive(context.Background(), "", nil, false) {
		t.Error("probeArchive(\"\") = true, want false for empty archiveRoot")
	}
}

func TestProbeArchiveUploadStream(t *testing.T) {
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/hello" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"version":"test"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srvOK.Close()
	if !probeArchive(context.Background(), "", branchdam.New(srvOK.URL, "0123456789abcdef0123456789abcdef"), true) {
		t.Errorf("probeArchive with authenticated hello 200 returned false, want true")
	}

	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srvErr.Close()
	if probeArchive(context.Background(), "", branchdam.New(srvErr.URL, "0123456789abcdef0123456789abcdef"), true) {
		t.Errorf("probeArchive with hello 500 returned true, want false")
	}

	if probeArchive(context.Background(), "", nil, true) {
		t.Error("probeArchive with nil client returned true, want false")
	}

	srvClosed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srvClosed.URL
	srvClosed.Close()
	if probeArchive(context.Background(), "", branchdam.New(closedURL, "0123456789abcdef0123456789abcdef"), true) {
		t.Error("probeArchive with closed server returned true, want false")
	}
}
