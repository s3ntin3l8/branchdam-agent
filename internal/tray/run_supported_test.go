//go:build windows || darwin

package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUIBinaryNameIsPlatformSpecific(t *testing.T) {
	got := uiBinaryName()
	if runtime.GOOS == "windows" {
		if got != "branchdam-agent-ui.exe" {
			t.Errorf("uiBinaryName() = %q, want %q on windows", got, "branchdam-agent-ui.exe")
		}
		return
	}
	if got != "branchdam-agent-ui" {
		t.Errorf("uiBinaryName() = %q, want %q on %s", got, "branchdam-agent-ui", runtime.GOOS)
	}
}

func TestResolveUIBinaryPathFindsSibling(t *testing.T) {
	dir := t.TempDir()
	trayPath := filepath.Join(dir, "tray-binary")
	uiPath := filepath.Join(dir, uiBinaryName())
	if err := os.WriteFile(trayPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uiPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveUIBinaryPath(trayPath)
	if err != nil {
		t.Fatalf("resolveUIBinaryPath: %v", err)
	}
	// resolveUIBinaryPath itself runs execPath through filepath.EvalSymlinks
	// before joining the sibling name, so the expected path must too: on
	// macOS, t.TempDir() returns a path under /var/folders/..., but /var is
	// itself a symlink to /private/var, matching internal/selfupdate's own
	// tests' precedent for the identical gotcha (install_test.go).
	wantUIPath, err := filepath.EvalSymlinks(uiPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantUIPath {
		t.Errorf("resolveUIBinaryPath() = %q, want %q", got, wantUIPath)
	}
}

func TestResolveUIBinaryPathMissingSibling(t *testing.T) {
	dir := t.TempDir()
	trayPath := filepath.Join(dir, "tray-binary")
	if err := os.WriteFile(trayPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveUIBinaryPath(trayPath); err == nil {
		t.Error("expected an error when the UI binary sibling doesn't exist")
	}
}
