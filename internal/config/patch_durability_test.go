package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPatchConcurrentCallsKeepEveryChange: concurrent Patches of
// different keys must all survive. Unserialised, each call read the file
// before the others renamed theirs into place and the last rename won.
func TestPatchConcurrentCallsKeepEveryChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# keep me\nagentId: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Patch(path, map[string]any{fmt.Sprintf("integrations.k%02d", i): i})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Patch: %v", err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Integrations map[string]int `yaml:"integrations"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Integrations) != n {
		t.Errorf("%d of %d concurrent changes survived:\n%s", len(doc.Integrations), n, raw)
	}
	if !strings.Contains(string(raw), "# keep me") {
		t.Error("comment lost")
	}
}

// TestPatchWritesThroughSymlink: a symlinked config.yaml keeps being a
// symlink; its target gets the change.
func TestPatchWritesThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	dotfiles := filepath.Join(dir, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "branchdam.yaml")
	if err := os.WriteFile(target, []byte("agentId: before # inline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("dotfiles", "branchdam.yaml"), link); err != nil {
		t.Fatal(err)
	}
	// A second hop, to exercise the chain.
	link2 := filepath.Join(dir, "config-link.yaml")
	if err := os.Symlink(link, link2); err != nil {
		t.Fatal(err)
	}

	if err := Patch(link2, map[string]any{"agentId": "after"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	for _, l := range []string{link, link2} {
		fi, err := os.Lstat(l)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer a symlink (mode %v, err %v)", l, fi.Mode(), err)
		}
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "agentId: after") || !strings.Contains(string(raw), "# inline") {
		t.Errorf("target not updated in place:\n%s", raw)
	}
	if entries, _ := os.ReadDir(dotfiles); len(entries) != 1 {
		t.Errorf("dotfiles dir holds %d entries, want just the target (temp file left behind?)", len(entries))
	}
}

// TestPatchCreatesMissingSymlinkTarget: a dangling link is resolved, and
// its target created, rather than the link replaced by a regular file.
func TestPatchCreatesMissingSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Patch(link, map[string]any{"agentId": "x"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if fi, _ := os.Lstat(link); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link replaced")
	}
	if raw, err := os.ReadFile(target); err != nil || !strings.Contains(string(raw), "agentId: x") {
		t.Errorf("target = %q, %v", raw, err)
	}
}

func TestPatchRejectsSymlinkLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if err := Patch(a, map[string]any{"agentId": "x"}); err == nil {
		t.Fatal("Patch through a symlink loop succeeded")
	}
}

// TestPatchFsyncsDirectory: the directory holding the replaced file is
// fsynced after the rename, and a failure there doesn't fail the save
// (the file is already in place).
func TestPatchFsyncsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var synced []string
	orig := syncDirFn
	t.Cleanup(func() { syncDirFn = orig })
	syncDirFn = func(dir string) error {
		synced = append(synced, dir)
		return errors.New("injected")
	}
	if err := Patch(path, map[string]any{"agentId": "x"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if len(synced) != 1 || synced[0] != filepath.Dir(path) {
		t.Errorf("synced dirs = %v, want [%s]", synced, filepath.Dir(path))
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "agentId: x") {
		t.Errorf("file = %q", raw)
	}
}
