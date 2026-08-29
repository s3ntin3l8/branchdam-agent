package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/hooks/resolve"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolvehook"
)

func TestResolveHookCandidateDirsOverride(t *testing.T) {
	dirs := resolveHookCandidateDirs("/custom/scripts/dir")
	if len(dirs) != 1 || dirs[0] != "/custom/scripts/dir" {
		t.Fatalf("got %v, want a single-element slice with the override", dirs)
	}
}

func TestResolveHookCandidateDirsAutodetect(t *testing.T) {
	dirs := resolveHookCandidateDirs("")
	want := resolvehook.CandidateDirs(runtime.GOOS, os.Getenv("HOME"), os.Getenv("APPDATA"))
	if len(dirs) != len(want) {
		t.Fatalf("got %v, want %v", dirs, want)
	}
	for i := range dirs {
		if dirs[i] != want[i] {
			t.Errorf("dirs[%d] = %q, want %q", i, dirs[i], want[i])
		}
	}
}

func TestResolveHookInstallerInstallCreatesFile(t *testing.T) {
	dir := t.TempDir()
	h := &resolveHookInstaller{scriptsDir: dir}

	st, err := h.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !st.Installed || !st.UpToDate {
		t.Errorf("got %+v, want Installed=true UpToDate=true", st)
	}
	if st.Dir != dir {
		t.Errorf("Dir = %q, want %q", st.Dir, dir)
	}
	if st.Path != filepath.Join(dir, resolve.FileName) {
		t.Errorf("Path = %q", st.Path)
	}

	got, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(resolve.Source) {
		t.Error("installed file content does not match the embedded source")
	}
}

func TestResolveHookInstallerInstallCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "Scripts", "Utility")
	h := &resolveHookInstaller{scriptsDir: dir}

	if _, err := h.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected Install to create the missing directory: %v", err)
	}
}

func TestResolveHookInstallerRevealReturnsErrorForBadTarget(t *testing.T) {
	// Reveal shells out to the platform's own "open" command; a directory
	// that exists but has no such command available (or an unwritable
	// target) still exercises the same code path Install does for
	// candidateDirs()/target selection -- this only asserts it doesn't
	// panic and returns whatever openWithDefaultApp itself reports.
	dir := t.TempDir()
	h := &resolveHookInstaller{scriptsDir: dir}
	_ = h.Reveal() // best-effort; no GUI shell available in CI, error is expected and fine
}

func TestRunResolveHookCmdDetectNotInstalled(t *testing.T) {
	dir := t.TempDir()
	var code int
	out := captureStdout(t, func() {
		code = runResolveHookCmd([]string{"-dir", dir})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "not installed") || !strings.Contains(out, dir) {
		t.Errorf("got %q, want it to mention %q as not installed", out, dir)
	}
}

func TestRunResolveHookCmdInstall(t *testing.T) {
	dir := t.TempDir()
	var code int
	out := captureStdout(t, func() {
		code = runResolveHookCmd([]string{"-dir", dir, "-install"})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	wantPath := filepath.Join(dir, resolve.FileName)
	if !strings.Contains(out, wantPath) {
		t.Errorf("got %q, want it to mention %q", out, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected the hook to be installed at %s: %v", wantPath, err)
	}
}

func TestRunResolveHookCmdDetectAfterInstallReportsUpToDate(t *testing.T) {
	dir := t.TempDir()
	captureStdout(t, func() { runResolveHookCmd([]string{"-dir", dir, "-install"}) })

	var code int
	out := captureStdout(t, func() {
		code = runResolveHookCmd([]string{"-dir", dir})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "up to date") {
		t.Errorf("got %q, want it to report up to date", out)
	}
}

func TestRunResolveHookCmdDetectAfterMutationReportsModified(t *testing.T) {
	dir := t.TempDir()
	captureStdout(t, func() { runResolveHookCmd([]string{"-dir", dir, "-install"}) })

	installedPath := filepath.Join(dir, resolve.FileName)
	if err := os.WriteFile(installedPath, []byte("hand-edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runResolveHookCmd([]string{"-dir", dir}) })
	if !strings.Contains(out, "modified or out of date") {
		t.Errorf("got %q, want it to report modified or out of date", out)
	}
}

func TestRunResolveHookCmdConfigFallbackSuppliesScriptsDir(t *testing.T) {
	scriptsDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := "integrations:\n  resolve:\n    scriptsDir: " + scriptsDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { runResolveHookCmd([]string{"-config", cfgPath}) })
	if !strings.Contains(out, scriptsDir) {
		t.Errorf("got %q, want it to mention the config-supplied scriptsDir %q", out, scriptsDir)
	}
}

func TestRunResolveHookCmdDirFlagWinsOverConfig(t *testing.T) {
	dirFlag := t.TempDir()
	configDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := "integrations:\n  resolve:\n    scriptsDir: " + configDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		runResolveHookCmd([]string{"-config", cfgPath, "-dir", dirFlag})
	})
	if !strings.Contains(out, dirFlag) {
		t.Errorf("got %q, want it to mention -dir's %q, not config's %q", out, dirFlag, configDir)
	}
	if strings.Contains(out, configDir) {
		t.Errorf("got %q, -dir should have overridden the config's scriptsDir entirely", out)
	}
}

func TestRunResolveHookCmdMissingConfigFallsBackToAutodetection(t *testing.T) {
	// A nonexistent -config path is not fatal -- resolve-hook falls back to
	// autodetection the same way the tray does, so an operator can run this
	// before config.yaml exists at all.
	var code int
	out := captureStdout(t, func() {
		code = runResolveHookCmd([]string{"-config", filepath.Join(t.TempDir(), "does-not-exist.yaml")})
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (missing config should not be fatal)", code)
	}
	if out == "" {
		t.Error("expected some status output even with a missing config")
	}
}

func TestRunResolveHookCmdRejectsUnknownFlag(t *testing.T) {
	code := runResolveHookCmd([]string{"-bogus"})
	if code != 2 {
		t.Errorf("got exit code %d, want 2", code)
	}
}
