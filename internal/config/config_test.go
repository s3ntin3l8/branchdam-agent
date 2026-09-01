package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureSlog swaps slog.Default() for a JSON-handler writing to buf
// for the duration of the test, restoring the prior default on cleanup
// so a misbehaving test can't leak its handler into a sibling. Used
// by the perm-warning tests to assert slog.Warn("config.yaml is
// group/world-readable; consider 'chmod 600'") was actually emitted
// (and not e.g. silently routed to /dev/null because the real default
// handler was rotated mid-test).
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf
}

func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.BaseURL != "http://localhost:8080" {
		t.Errorf("expected default base URL, got %s", cfg.Server.BaseURL)
	}
	if cfg.AgentID != "" {
		t.Errorf("expected empty default agentId, got %s", cfg.AgentID)
	}
	if len(cfg.PathMappings) != 0 {
		t.Errorf("expected no default path mappings, got %v", cfg.PathMappings)
	}
}

func TestLoadOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
server:
  baseUrl: "https://branchdam.example.com"
  apiKey: "0123456789abcdef0123456789abcdef"
agentId: "workstation-01"
pathMappings:
  - workstationPath: "D:\\Photos"
    containerPath: "/storage/archive"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.BaseURL != "https://branchdam.example.com" {
		t.Errorf("expected overridden base URL, got %s", cfg.Server.BaseURL)
	}
	if cfg.Server.APIKey != "0123456789abcdef0123456789abcdef" {
		t.Errorf("expected overridden API key, got %s", cfg.Server.APIKey)
	}
	if cfg.AgentID != "workstation-01" {
		t.Errorf("expected overridden agentId, got %s", cfg.AgentID)
	}
	if len(cfg.PathMappings) != 1 || cfg.PathMappings[0].ContainerPath != "/storage/archive" {
		t.Errorf("expected one path mapping to /storage/archive, got %v", cfg.PathMappings)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TEST_API_KEY", "secret-value")

	result := expandEnv("apiKey: ${TEST_API_KEY}")
	if result != "apiKey: secret-value" {
		t.Errorf("expected env expansion, got %s", result)
	}
}

func TestExpandEnvMissing(t *testing.T) {
	result := expandEnv("apiKey: ${MISSING_VARXYZ}")
	if result != "apiKey: ${MISSING_VARXYZ}" {
		t.Errorf("expected unchanged for missing var, got %s", result)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

// TestTrayAndSelfUpdateDefaults checks the bare Go zero value of Config,
// not what Load() actually hands an operator -- see
// TestLoadSelfUpdateEnabledByDefault for that. The zero value keeping
// SelfUpdate.Enabled false is just ordinary Go struct semantics: it's
// defaultConfig(), not the zero value, that turns checks on by default.
func TestTrayAndSelfUpdateDefaults(t *testing.T) {
	var cfg Config
	if got := cfg.Tray.StatusAddrOrDefault(); got != DefaultStatusAddr {
		t.Errorf("got %q, want default %q", got, DefaultStatusAddr)
	}
	if got := cfg.SelfUpdate.RepoOrDefault(); got != DefaultSelfUpdateRepo {
		t.Errorf("got %q, want default %q", got, DefaultSelfUpdateRepo)
	}
	if cfg.Tray.StartOnLogin {
		t.Error("StartOnLogin must default to false")
	}
	if cfg.SelfUpdate.Enabled {
		t.Error("the zero-value Config's SelfUpdate.Enabled must be false")
	}
}

// TestLoadSelfUpdateEnabledByDefault pins the operator-visible default:
// a config file that never mentions selfUpdate at all still gets update
// CHECKS on (a read-only GitHub API call, never a download or a binary
// write) -- only APPLYING an update is gated behind an always-explicit
// action (a tray menu click, or `update`'s confirmation/-yes), which this
// flag does not by itself authorize. An operator who wants zero outbound
// GitHub traffic sets selfUpdate.enabled: false explicitly.
func TestLoadSelfUpdateEnabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agentId: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SelfUpdate.Enabled {
		t.Error("SelfUpdate.Enabled must default to true when the config file doesn't mention selfUpdate at all")
	}
}

func TestLoadSelfUpdateExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "selfUpdate:\n  enabled: false\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SelfUpdate.Enabled {
		t.Error("an explicit selfUpdate.enabled: false in config must override the on-by-default check")
	}
}

func TestLoadTrayAndSelfUpdateOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
tray:
  statusAddr: "127.0.0.1:9999"
  startOnLogin: true
selfUpdate:
  enabled: true
  repo: "someone/fork"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tray.StatusAddrOrDefault() != "127.0.0.1:9999" {
		t.Errorf("got %q", cfg.Tray.StatusAddrOrDefault())
	}
	if !cfg.Tray.StartOnLogin {
		t.Error("expected StartOnLogin=true")
	}
	if !cfg.SelfUpdate.Enabled || cfg.SelfUpdate.RepoOrDefault() != "someone/fork" {
		t.Errorf("got %+v", cfg.SelfUpdate)
	}
}

func TestLoadRequireUnbuffered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
ingest:
  requireUnbuffered: true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Ingest.RequireUnbuffered {
		t.Error("expected RequireUnbuffered=true")
	}
}

func TestLoadPruneConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
prune:
  enabled: true
  minAgeHours: 48
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Prune.Enabled || cfg.Prune.MinAgeHours != 48 {
		t.Errorf("got %+v, want Enabled=true MinAgeHours=48", cfg.Prune)
	}
}

// TestLoadIntegrationsDryRunEnabledByDefault mirrors
// TestLoadSelfUpdateEnabledByDefault: a config file that never mentions
// integrations at all must still come out with Luminar.DryRun true -- a
// fresh install resolves and logs what a sync would emit and contacts no
// server until an operator turns dry-run off explicitly.
func TestLoadIntegrationsDryRunEnabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agentId: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Integrations.Luminar.DryRun {
		t.Error("Integrations.Luminar.DryRun must default to true when the config file doesn't mention integrations at all")
	}
}

// TestLoadIntegrationsDryRunExplicitlyDisabled is the override side of the
// above -- an explicit dryRun: false must survive Load, exactly like
// selfUpdate.enabled: false does.
func TestLoadIntegrationsDryRunExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "integrations:\n  luminar:\n    enabled: true\n    dryRun: false\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Integrations.Luminar.DryRun {
		t.Error("an explicit integrations.luminar.dryRun: false in config must override the on-by-default dry run")
	}
	if !cfg.Integrations.Luminar.Enabled {
		t.Error("expected Enabled=true")
	}
}

func TestLoadIntegrationsOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
integrations:
  nodeIndexPath: "/data/node-index.json"
  luminar:
    enabled: true
    catalogPath: "/data/catalog.luminarneo"
    dryRun: false
    syncIntervalMinutes: 15
    timeoutSecs: 45
  resolve:
    scriptsDir: "/custom/Scripts/Utility"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Integrations.NodeIndexPath != "/data/node-index.json" {
		t.Errorf("got %q", cfg.Integrations.NodeIndexPath)
	}
	if !cfg.Integrations.Luminar.Enabled || cfg.Integrations.Luminar.CatalogPath != "/data/catalog.luminarneo" {
		t.Errorf("got %+v", cfg.Integrations.Luminar)
	}
	if cfg.Integrations.Luminar.SyncIntervalMinutes != 15 || cfg.Integrations.Luminar.TimeoutSecs != 45 {
		t.Errorf("got %+v", cfg.Integrations.Luminar)
	}
	if cfg.Integrations.Resolve.ScriptsDir != "/custom/Scripts/Utility" {
		t.Errorf("got %q", cfg.Integrations.Resolve.ScriptsDir)
	}
}

func TestLoadPruneConfigDefaultsDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  baseUrl: http://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Prune.Enabled {
		t.Error("expected Prune.Enabled=false when the prune block is entirely absent")
	}
}

// TestLoadWorldReadableConfigWithAPIKeyWarns is the headline assertion
// for issue #97 / audit S-5: a 0o644 config.yaml carrying a real
// server.apiKey must produce slog.Warn("config.yaml is
// group/world-readable; consider 'chmod 600'") after a successful
// Load. The config still loads -- the default-mode check is a warning,
// not a refusal, so an existing deployment that just noticed the gap
// keeps starting while the operator fixes it.
func TestLoadWorldReadableConfigWithAPIKeyWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  baseUrl: "https://branchdam.example.com"
  apiKey: "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := captureSlog(t)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load on a world-readable config with an apiKey must still succeed (default mode is warn, not refuse): %v", err)
	}
	if cfg.Server.APIKey == "" {
		t.Error("expected apiKey to round-trip through Load")
	}

	// Walk the JSON log line(s) and look for the warning -- structured
	// matching rather than strings.Contains so a typo or extra wrapping
	// in the warning call is caught (it would be emitted under a
	// different msg key).
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("slog emitted a non-JSON line: %q", line)
		}
		if rec["msg"] == "config.yaml is group/world-readable; consider 'chmod 600'" {
			found = rec
			break
		}
	}
	if found == nil {
		t.Fatalf("expected slog.Warn(\"config.yaml is group/world-readable; consider 'chmod 600'\") to be emitted; got log: %s", logBuf.String())
	}
	if found["level"] != "WARN" {
		t.Errorf("expected level=WARN, got %v", found["level"])
	}
	if found["path"] != path {
		t.Errorf("expected path=%q, got %v", path, found["path"])
	}
}

// TestLoadWorldReadableConfigWithoutAPIKeyIsSilent pins the "no-op when
// there's no secret to protect" half of checkFilePermissions: a
// world-readable config without a real apiKey must NOT warn, because
// the file carries nothing the operator can lock down by chmod. This
// keeps the warning meaningful -- a 0o644 file that already passes the
// "no real secret" check shouldn't be surfaced to the operator.
func TestLoadWorldReadableConfigWithoutAPIKeyIsSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  baseUrl: http://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := captureSlog(t)

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if strings.Contains(logBuf.String(), "group/world-readable") {
		t.Errorf("expected no perm warning for a 0o644 config without a real apiKey; got log: %s", logBuf.String())
	}
}

// TestLoadStrictModeRefusesWorldReadable is the strict-mode half of
// issue #97: with strictConfigPermissions: true, a 0o644 config.yaml
// carrying a real server.apiKey must make Load return a hard error --
// the agent refuses to start until the operator chmods 600. The
// message must name the path so an operator staring at a generic
// "config error" output can find the file, and must include the
// current mode so the operator can confirm chmod took effect on retry.
// The remediation suggestion must mention the YAML toggle, since the
// YAML value is the source of the strict setting in this test.
func TestLoadStrictModeRefusesWorldReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  baseUrl: "https://branchdam.example.com"
  apiKey: "0123456789abcdef0123456789abcdef"
strictConfigPermissions: true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected Load to return an error in strict mode with a world-readable config")
	}
	if !strings.Contains(err.Error(), "strict mode") {
		t.Errorf("error must name strict mode so an operator can find the toggle; got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the offending path; got: %v", err)
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error must suggest the fix; got: %v", err)
	}
	if !strings.Contains(err.Error(), "strictConfigPermissions: false") {
		t.Errorf("error must mention the YAML toggle when YAML is the source; got: %v", err)
	}
}

// TestLoadStrictModeRefusesWhenEnvForcesIt pins the env-as-source path
// of issue #97's strict toggle: when the env var is what turned strict
// on (the env wins over the YAML value, including an explicit false),
// the remediation message must point at the env var, NOT the YAML
// toggle -- a "set strictConfigPermissions: false" suggestion is a
// no-op when the env is the trigger (Hermes review on #126).
func TestLoadStrictModeRefusesWhenEnvForcesIt(t *testing.T) {
	t.Setenv(StrictConfigPermissionsEnv, "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// strictConfigPermissions explicitly false in YAML; the env var
	// must still make Load refuse.
	content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
strictConfigPermissions: false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected Load to refuse when env forces strict even with yaml strictConfigPermissions: false")
	}
	if !strings.Contains(err.Error(), "unset "+StrictConfigPermissionsEnv) {
		t.Errorf("error must tell operator to unset the env var when env is the source; got: %v", err)
	}
	if strings.Contains(err.Error(), "set strictConfigPermissions: false") {
		t.Errorf("error must NOT suggest the YAML toggle when env is the source (it would be a no-op); got: %v", err)
	}
}

// TestLoadStrictModeViaEnv is the env-var half of issue #97's strict
// toggle. The env var BRANCHDAM_AGENT_STRICT_CONFIG_PERMISSIONS lets
// a CI/scripted environment enforce the same refusal without editing
// config.yaml first, and must win over the YAML value (a config that
// explicitly sets the flag to false can still be hard-failed by the
// env, and vice versa).
func TestLoadStrictModeViaEnv(t *testing.T) {
	t.Run("env-true-overrides-yaml-false", func(t *testing.T) {
		t.Setenv(StrictConfigPermissionsEnv, "1")
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		// strictConfigPermissions explicitly false in YAML; the env
		// var must still make Load refuse.
		content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
strictConfigPermissions: false
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Error("expected Load to refuse when env sets strict even with yaml strictConfigPermissions: false")
		}
	})

	t.Run("env-false-overrides-yaml-true", func(t *testing.T) {
		t.Setenv(StrictConfigPermissionsEnv, "0")
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		// strictConfigPermissions explicitly true in YAML; the env
		// var must still make Load warn (not refuse).
		content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
strictConfigPermissions: true
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		logBuf := captureSlog(t)
		if _, err := Load(path); err != nil {
			t.Errorf("expected Load to warn, not refuse, when env unsets strict even with yaml strictConfigPermissions: true: %v", err)
		}
		if !strings.Contains(logBuf.String(), "group/world-readable") {
			t.Errorf("expected the same warning as default mode; got log: %s", logBuf.String())
		}
	})

	t.Run("env-unrecognized-falls-back-with-warning", func(t *testing.T) {
		// A typo'd env value (trailing space, "2", a synonym the
		// helper doesn't recognize) must NOT silently behave as
		// "env unset" -- the env-wins contract is the value of the
		// override, not its interpretation (Hermes review on #126).
		// An unrecognized value falls back to the YAML value and
		// emits a one-shot slog.Warn so the operator can find the
		// typo.
		t.Setenv(StrictConfigPermissionsEnv, "2")
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		// YAML says strict; env is unrecognized; result must
		// follow YAML (strict), and the unrecognized-env warning
		// must be emitted.
		content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
strictConfigPermissions: true
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		logBuf := captureSlog(t)
		if _, err := Load(path); err == nil {
			t.Error("expected Load to refuse when env is unrecognized but yaml strictConfigPermissions is true")
		}
		if !strings.Contains(logBuf.String(), "unrecognized value for "+StrictConfigPermissionsEnv) {
			t.Errorf("expected unrecognized-env warning; got log: %s", logBuf.String())
		}
	})
}

// TestLoadModeZeroSixZeroWithAPIKeyIsClean confirms the false-positive
// half of the warning: a config.yaml at mode 0o600 (Patch's own output
// mode) with a real apiKey must NOT warn -- the file is already
// properly locked down, and the warning would be noise that erodes
// operator trust in every subsequent perm warning.
func TestLoadModeZeroSixZeroWithAPIKeyIsClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	logBuf := captureSlog(t)

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(logBuf.String(), "group/world-readable") {
		t.Errorf("expected no perm warning for a 0o600 config with a real apiKey; got log: %s", logBuf.String())
	}
}

// TestLoadGroupOnlyReadableConfigStillWarns pins the wording change
// requested on #126: the 0o077 mask also trips on group-only modes
// (e.g. 0o640 -- group readable, world not), so the warning must
// describe "group/world-readable", not "world-readable", to match what
// the mask actually checks. A 0o640 file with a real apiKey trips the
// same warning as 0o644.
func TestLoadGroupOnlyReadableConfigStillWarns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}

	logBuf := captureSlog(t)

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(logBuf.String(), "group/world-readable") {
		t.Errorf("expected group/world-readable warning for a 0o640 config; got log: %s", logBuf.String())
	}
}

// TestLoadSkipsCheckOnWindows pins the platform gate added on
// #126: a 0o644 file on Windows would always trip the 0o077 mask
// (os.FileMode.Perm() returns 0o666 for any non-readonly file on
// Windows, regardless of ACLs), so the check is a no-op on Windows
// by design. This test runs on every platform; on POSIX it is
// effectively a no-op (the file's real perm bits determine the
// outcome), and on Windows it pins the "no warning" guarantee. The
// guarded-by-runtime.GOOS logic is unit-tested directly so the
// platform branch is exercised on at least one OS regardless.
func TestLoadSkipsCheckOnWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  apiKey: "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := captureSlog(t)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.APIKey == "" {
		t.Error("expected apiKey to round-trip through Load")
	}
	if runtime.GOOS == "windows" {
		// On Windows, the check is intentionally a no-op -- an
		// operator who wants file-level access control uses
		// Explorer > Properties > Security, not chmod.
		if strings.Contains(logBuf.String(), "group/world-readable") {
			t.Errorf("expected no perm warning on Windows; got log: %s", logBuf.String())
		}
	}
	// On POSIX the warning is expected; the platform-specific
	// assertion is the no-warning half above.
}
