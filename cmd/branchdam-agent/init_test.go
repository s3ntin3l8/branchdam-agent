package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

func TestRunInitCmdWritesStarterConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	got := runInitCmd([]string{"-config", path})
	if got != 0 {
		t.Fatalf("runInitCmd = %d, want 0", got)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load written config: %v", err)
	}
	if cfg.Server.BaseURL == "" {
		t.Error("expected a non-empty default server.baseUrl in the starter config")
	}
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Errorf("starter config should have no Validate() problems, got %v", problems)
	}
}

func TestRunInitCmdRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agentId: pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := runInitCmd([]string{"-config", path})
	if got != 1 {
		t.Fatalf("runInitCmd = %d, want 1 (refuse to overwrite)", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "agentId: pre-existing\n" {
		t.Error("existing config was overwritten despite missing -force")
	}
}

func TestRunInitCmdForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agentId: pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := runInitCmd([]string{"-config", path, "-force"})
	if got != 0 {
		t.Fatalf("runInitCmd = %d, want 0", got)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentID == "pre-existing" {
		t.Error("expected -force to overwrite the pre-existing config")
	}
}

func TestRunInitCmdCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "sub", "config.yaml")

	got := runInitCmd([]string{"-config", path})
	if got != 0 {
		t.Fatalf("runInitCmd = %d, want 0", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected config to exist at %s: %v", path, err)
	}
}
