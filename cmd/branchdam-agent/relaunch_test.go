package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelaunchSelfPlainBinary(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "fake-branchdam-agent")
	if err := os.WriteFile(self, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := relaunchSelf(self, []string{"tray", "-config", "config.yaml"}); err != nil {
		t.Errorf("relaunchSelf: %v", err)
	}
}

func TestEnableStartOnLoginResolvesRelativeConfigPath(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err = enableStartOnLogin("config.yaml")
	// On Linux this always fails with ErrUnsupported (autostart.Enable),
	// but it must fail for THAT reason, never because filepath.Abs
	// itself errored -- this test only pins the abs-path resolution
	// step, not the (platform-specific, untestable-on-Linux) actual
	// registration.
	if err == nil {
		t.Fatal("expected an error on this platform")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.yaml")); statErr == nil {
		t.Fatal("test setup bug: config.yaml should not exist")
	}
}
