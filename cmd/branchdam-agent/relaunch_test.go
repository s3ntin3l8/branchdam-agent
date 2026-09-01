package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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

// TestRelaunchMacBundleOpenArgsRequiresNewInstance pins the argv passed
// to `open` for the macOS bundled relaunch path. The -n flag is
// required: relaunchSelf is called while the old process is still
// alive, so without -n LaunchServices activates the old instance and
// ignores --args. The port conflict that -n would normally cause is
// mitigated by stop()+wg.Wait() before relaunchSelf. See issue #107.
// Extracted as a pure function so the test runs on every CI host, not
// just darwin.
func TestRelaunchMacBundleOpenArgsRequiresNewInstance(t *testing.T) {
	bundle := "/Applications/branchdam-agent.app"
	args := []string{"tray", "-config", "config.yaml"}

	got := relaunchMacBundleOpenArgs(bundle, args)

	if !slices.Contains(got, "-n") {
		t.Errorf("relaunchMacBundleOpenArgs must include -n (old process still alive at relaunch time); got: %v", got)
	}

	want := []string{"-n", "-a", bundle, "--args", "tray", "-config", "config.yaml"}
	if !slices.Equal(got, want) {
		t.Errorf("argv mismatch\n  got:  %v\n  want: %v", got, want)
	}
}

// TestRelaunchMacBundleCallsStart pins the call to startRelaunchCmd
// from relaunchMacBundle. Substitutes the package-level var with a
// recording stub so the test runs on Linux (no `open` binary to
// actually exec) but still asserts that the wrapper path is wired
// up correctly. See issue #107.
func TestRelaunchMacBundleCallsStart(t *testing.T) {
	orig := startRelaunchCmd
	t.Cleanup(func() { startRelaunchCmd = orig })

	var calls atomic.Int32
	var sawArgv atomic.Value
	startRelaunchCmd = func(cmd *exec.Cmd) error {
		calls.Add(1)
		sawArgv.Store(cmd.Args)
		return nil
	}

	bundle := "/Applications/branchdam-agent.app"
	if err := relaunchMacBundle(bundle, []string{"tray"}); err != nil {
		t.Fatalf("relaunchMacBundle: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("startRelaunchCmd call count: got %d, want 1", got)
	}
	gotArgv, _ := sawArgv.Load().([]string)
	wantArgv := []string{"open", "-n", "-a", bundle, "--args", "tray"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Errorf("argv at exec: got %v, want %v", gotArgv, wantArgv)
	}
}

// TestRelaunchMacBundleStartError asserts that the call site wraps
// a start error with the bundle path and the `open -a` command line,
// rather than swallowing it. The wrapping is a property of
// relaunchMacBundle itself, not the test stub. See issue #107.
func TestRelaunchMacBundleStartError(t *testing.T) {
	orig := startRelaunchCmd
	t.Cleanup(func() { startRelaunchCmd = orig })

	startRelaunchCmd = func(cmd *exec.Cmd) error {
		return errors.New("simulated spawn failure")
	}

	bundle := "/Applications/branchdam-agent.app"
	err := relaunchMacBundle(bundle, nil)
	if err == nil {
		t.Fatal("expected an error from relaunchMacBundle")
	}
	msg := err.Error()
	for _, want := range []string{bundle, "open -n -a", "simulated spawn failure"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should contain %q; got: %v", want, err)
		}
	}
}
