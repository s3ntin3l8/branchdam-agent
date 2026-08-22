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
		t.Error("SelfUpdate.Enabled must default to false")
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
