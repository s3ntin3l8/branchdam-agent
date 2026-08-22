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

	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected default :8080, got %s", cfg.ListenAddr)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected default info, got %s", cfg.LogLevel)
	}
	if cfg.HTTP.ReadTimeoutSecs != 15 {
		t.Errorf("expected default 15, got %d", cfg.HTTP.ReadTimeoutSecs)
	}
}

func TestLoadOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
listenAddr: ":9090"
logLevel: "debug"
http:
  readTimeoutSecs: 30
  writeTimeoutSecs: 30
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("expected :9090, got %s", cfg.ListenAddr)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected debug, got %s", cfg.LogLevel)
	}
	if cfg.HTTP.ReadTimeoutSecs != 30 {
		t.Errorf("expected 30, got %d", cfg.HTTP.ReadTimeoutSecs)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TEST_PORT", ":7070")

	result := expandEnv("listenAddr: ${TEST_PORT}")
	if result != "listenAddr: :7070" {
		t.Errorf("expected env expansion, got %s", result)
	}
}

func TestExpandEnvMissing(t *testing.T) {
	result := expandEnv("listenAddr: ${MISSING_VARXYZ}")
	if result != "listenAddr: ${MISSING_VARXYZ}" {
		t.Errorf("expected unchanged for missing var, got %s", result)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
