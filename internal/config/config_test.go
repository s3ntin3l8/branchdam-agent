package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
