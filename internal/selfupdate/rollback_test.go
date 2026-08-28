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

func TestRollbackInfo(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	layout := InstallLayout{Primary: primary}

	if _, ok := RollbackInfo(layout); ok {
		t.Fatal("expected ok=false before any backup exists")
	}

	if err := os.WriteFile(primary+rollbackSuffix, []byte("old bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("1.5.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	version, ok := RollbackInfo(layout)
	if !ok || version != "1.5.0" {
		t.Errorf("RollbackInfo() = (%q, %v), want (\"1.5.0\", true)", version, ok)
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

// TestRollbackPartialFailureLeavesUnprocessedTargetsIntact is a
// regression test for a Hermes review finding: a mid-loop restore
// failure must never touch a target it hasn't reached yet.
func TestRollbackPartialFailureLeavesUnprocessedTargetsIntact(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent.exe")
	sibling := filepath.Join(dir, "branchdam-agent-tray.exe")
	writeAppliedState(t, primary, "old primary bytes", "1.0.0")
	writeAppliedState(t, sibling, "old sibling bytes", "1.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("1.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, Siblings: []string{sibling}}

	// Corrupt the sibling's backup (processed first) so its restore fails
	// before primary (processed last) is ever touched.
	if err := os.Remove(sibling + rollbackSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sibling+rollbackSuffix, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Rollback(layout); err == nil {
		t.Fatal("expected Rollback to fail on the sibling's corrupted backup")
	}

	if got, _ := os.ReadFile(primary); string(got) != "current (new) binary bytes" {
		t.Errorf("primary changed despite the failure happening on the sibling, processed first: %q", got)
	}
	if !HasRollback(layout) {
		t.Error("expected HasRollback=true -- primary's own backup+sidecar are untouched")
	}
}

// TestRollbackRetrySkipsAlreadyRestoredTargets is a regression test for a
// Hermes review finding: restoreOne must tolerate an already-consumed
// backup as a no-op, or a retry after a mid-loop failure could never
// succeed for a target that already restored successfully on the first
// attempt.
func TestRollbackRetrySkipsAlreadyRestoredTargets(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent.exe")
	sibling := filepath.Join(dir, "branchdam-agent-tray.exe")
	writeAppliedState(t, primary, "old primary bytes", "1.0.0")
	writeAppliedState(t, sibling, "old sibling bytes", "1.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("1.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, Siblings: []string{sibling}}

	// Corrupt PRIMARY's backup (processed last) so the sibling succeeds
	// -- and its own backup is consumed -- before the failure happens.
	if err := os.Remove(primary + rollbackSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(primary+rollbackSuffix, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Rollback(layout); err == nil {
		t.Fatal("expected Rollback to fail on primary's corrupted backup")
	}
	if got, _ := os.ReadFile(sibling); string(got) != "old sibling bytes" {
		t.Errorf("sibling not restored despite succeeding before the failure: %q", got)
	}
	if _, err := os.Stat(sibling + rollbackSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("expected the sibling's backup to already be removed after its own successful restore")
	}

	// Fix primary's backup and retry -- the sibling has NO backup left at
	// this point, so restoreOne must treat that as already-done rather
	// than an error, or this retry would incorrectly fail too.
	if err := os.Remove(primary + rollbackSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primary+rollbackSuffix, []byte("old primary bytes"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Rollback(layout); err != nil {
		t.Fatalf("expected the retry to succeed once primary's backup is fixed, got: %v", err)
	}
	if got, _ := os.ReadFile(primary); string(got) != "old primary bytes" {
		t.Errorf("primary content after retry = %q", got)
	}
	if HasRollback(layout) {
		t.Error("expected HasRollback=false once the retry fully succeeds")
	}
}

// TestRollbackInfoPlistFailureLeavesSidecarForRetry is a regression test
// for a Hermes review finding: the version sidecar must survive an
// Info.plist write failure (it happens after every target is already
// restored) so a retry can still find PreviousVersion and finish the job
// rather than losing track of what version to roll back to.
func TestRollbackInfoPlistFailureLeavesSidecarForRetry(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	writeAppliedState(t, primary, "old binary bytes", "3.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("3.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, InfoPlist: filepath.Join(dir, "nonexistent-dir", "Info.plist")}

	if _, err := Rollback(layout); err == nil {
		t.Fatal("expected Rollback to fail when Info.plist's directory doesn't exist")
	}

	if got, _ := os.ReadFile(primary); string(got) != "old binary bytes" {
		t.Errorf("primary content = %q, want the restored old bytes even though the later Info.plist write failed", got)
	}
	if _, err := PreviousVersion(layout); err != nil {
		t.Errorf("expected the version sidecar to survive an Info.plist write failure so a retry can still find it, got: %v", err)
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
