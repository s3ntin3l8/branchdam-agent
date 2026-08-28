package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasRollbackFalseWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	layout := InstallLayout{Primary: filepath.Join(dir, "branchdam-agent")}
	if HasRollback(layout) {
		t.Error("expected HasRollback=false when no .previous backup exists")
	}
}

func TestHasRollbackFalseWithBackupButNoSidecar(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	if err := os.WriteFile(primary+rollbackSuffix, []byte("old binary bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary}
	if HasRollback(layout) {
		t.Error("expected HasRollback=false when the version sidecar is missing, even with a .previous backup present")
	}
}

func TestPreviousVersionNoRollback(t *testing.T) {
	dir := t.TempDir()
	layout := InstallLayout{Primary: filepath.Join(dir, "branchdam-agent")}
	if _, err := PreviousVersion(layout); err == nil {
		t.Error("expected an error when no rollback is available")
	}
}

// writeAppliedState simulates what a prior Apply left behind: the
// current binary at target, its pre-update content saved at
// target+rollbackSuffix, and the version sidecar recording that saved
// content's version.
func writeAppliedState(t *testing.T, target, previousContent, previousVersion string) {
	t.Helper()
	if err := os.WriteFile(target, []byte("current (new) binary bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+rollbackSuffix, []byte(previousContent), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackRestoresPrimaryAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	writeAppliedState(t, primary, "old binary bytes", "1.2.3")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary}

	if !HasRollback(layout) {
		t.Fatal("expected HasRollback=true before Rollback runs")
	}

	gotVersion, err := Rollback(layout)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if gotVersion != "1.2.3" {
		t.Errorf("Rollback version = %q, want %q", gotVersion, "1.2.3")
	}

	restored, err := os.ReadFile(primary)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != "old binary bytes" {
		t.Errorf("primary content after rollback = %q, want the backed-up old bytes", restored)
	}

	if _, err := os.Stat(primary + rollbackSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("expected the .previous backup to be removed after a successful rollback")
	}
	if _, err := os.Stat(primary + rollbackVersionSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("expected the version sidecar to be removed after a successful rollback")
	}
	if HasRollback(layout) {
		t.Error("expected HasRollback=false immediately after a successful rollback -- nothing left to roll back to")
	}
}

func TestRollbackRestoresSiblingsAndPrimary(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent.exe")
	sibling := filepath.Join(dir, "branchdam-agent-tray.exe")
	writeAppliedState(t, primary, "old primary bytes", "1.0.0")
	writeAppliedState(t, sibling, "old sibling bytes", "1.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("1.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, Siblings: []string{sibling}}

	if _, err := Rollback(layout); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	for _, want := range []struct{ path, content string }{
		{primary, "old primary bytes"},
		{sibling, "old sibling bytes"},
	} {
		got, err := os.ReadFile(want.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want.content {
			t.Errorf("%s content = %q, want %q", want.path, got, want.content)
		}
	}
}

func TestRollbackRewritesInfoPlist(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	writeAppliedState(t, primary, "old binary bytes", "2.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "Info.plist")
	if err := os.WriteFile(plistPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, InfoPlist: plistPath}

	if _, err := Rollback(layout); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "2.0.0") {
		t.Errorf("Info.plist after rollback = %q, want it to mention version 2.0.0", got)
	}
}
