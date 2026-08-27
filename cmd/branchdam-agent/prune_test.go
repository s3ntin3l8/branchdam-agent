package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunPruneCmdRefusesWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  apiKey: test-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := runPruneCmd([]string{"-config", cfgPath})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (prune.enabled defaults to false)", code)
	}
}
