package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunRequiresFlags(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
}

func TestRunWritesBundle(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "src-binary")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(dir, "branchdam-agent.app")

	got := run([]string{"-app", appDir, "-binary", bin, "-version", "v1.2.3"})
	if got != 0 {
		t.Fatalf("run() = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(appDir, "Contents", "Info.plist")); err != nil {
		t.Errorf("Info.plist not written: %v", err)
	}
}
