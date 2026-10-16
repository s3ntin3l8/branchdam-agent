package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSwap simulates go-selfupdate's per-target swap with
// OldSavePath=<target>.previous: the current binary is moved aside to its
// backup and the new content takes its place.
func fakeSwap(target, newContent string) error {
	if err := os.Rename(target, target+rollbackSuffix); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(newContent), 0o755)
}

func threeTargetLayout(t *testing.T) InstallLayout {
	t.Helper()
	dir := t.TempDir()
	layout := InstallLayout{
		Primary: filepath.Join(dir, "branchdam-agent.exe"),
		Siblings: []string{
			filepath.Join(dir, "branchdam-agent-tray.exe"),
			filepath.Join(dir, "branchdam-agent-ui.exe"),
		},
	}
	for _, target := range layout.orderedTargets() {
		if err := os.WriteFile(target, []byte("v1 "+filepath.Base(target)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return layout
}

func assertAllAtV1(t *testing.T, layout InstallLayout) {
	t.Helper()
	for _, target := range layout.orderedTargets() {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if want := "v1 " + filepath.Base(target); string(got) != want {
			t.Errorf("%s = %q, want %q (mixed versions after a failed apply)", filepath.Base(target), got, want)
		}
	}
}

func TestApplyAllTargetsWritesSidecarOnlyAfterEverySwap(t *testing.T) {
	layout := threeTargetLayout(t)
	sidecar := layout.Primary + rollbackVersionSuffix

	err := applyAllTargets(layout, "1.0.0", func(target string) error {
		if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rollback sidecar exists while %s is still being swapped", filepath.Base(target))
		}
		return fakeSwap(target, "v2")
	})
	if err != nil {
		t.Fatalf("applyAllTargets: %v", err)
	}
	if v, ok := RollbackInfo(layout); !ok || v != "1.0.0" {
		t.Errorf("RollbackInfo() = (%q, %v), want (\"1.0.0\", true)", v, ok)
	}
}

// TestApplyAllTargetsPartialFailureRestoresSwappedTargets is the regression
// test for the audit finding: a failure on the primary after the siblings
// already swapped used to leave the siblings on the new version and a
// rollback sidecar advertising the old one.
func TestApplyAllTargetsPartialFailureRestoresSwappedTargets(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		layout := threeTargetLayout(t)
		targets := layout.orderedTargets()
		boom := errors.New("simulated download failure")

		err := applyAllTargets(layout, "1.0.0", func(target string) error {
			if target == targets[failAt] {
				return boom
			}
			return fakeSwap(target, "v2")
		})
		if !errors.Is(err, boom) {
			t.Fatalf("failAt=%d: err = %v, want it to wrap the target failure", failAt, err)
		}
		assertAllAtV1(t, layout)
		if v, ok := RollbackInfo(layout); ok {
			t.Errorf("failAt=%d: RollbackInfo() = (%q, true) after a failed apply, want no rollback advertised", failAt, v)
		}
		for _, target := range targets[:failAt] {
			if _, err := os.Stat(target + rollbackSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("failAt=%d: %s backup left behind after it was restored", failAt, filepath.Base(target))
			}
		}
	}
}

// withPriorRollback gives every target a ".previous" backup and the
// sidecar labelling them 0.9.0, as a prior successful Apply leaves them.
func withPriorRollback(t *testing.T, layout InstallLayout) {
	t.Helper()
	for _, target := range layout.orderedTargets() {
		if err := os.WriteFile(target+rollbackSuffix, []byte("v0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(layout.Primary+rollbackVersionSuffix, []byte("0.9.0"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestApplyAllTargetsClearsStaleSidecarOnceASwapHappened covers a prior
// Apply's rollback: its sidecar must not survive a failed apply that has
// already overwritten some of that rollback's backups.
func TestApplyAllTargetsClearsStaleSidecarOnceASwapHappened(t *testing.T) {
	layout := threeTargetLayout(t)
	withPriorRollback(t, layout)

	err := applyAllTargets(layout, "1.0.0", func(target string) error {
		if target == layout.Primary {
			return errors.New("simulated primary failure")
		}
		return fakeSwap(target, "v2")
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	assertAllAtV1(t, layout)
	if v, ok := RollbackInfo(layout); ok {
		t.Errorf("RollbackInfo() = (%q, true), want the stale 0.9.0 rollback withdrawn", v)
	}
}

func TestApplyAllTargetsSidecarFailureRestoresEveryTarget(t *testing.T) {
	layout := threeTargetLayout(t)
	sidecar := layout.Primary + rollbackVersionSuffix

	err := applyAllTargets(layout, "1.0.0", func(target string) error {
		if err := fakeSwap(target, "v2"); err != nil {
			return err
		}
		if target == layout.Primary {
			// Block the sidecar write that follows the last swap.
			return os.MkdirAll(filepath.Join(sidecar, "blocker"), 0o755)
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the sidecar write failure to be reported")
	}
	assertAllAtV1(t, layout)
}

// TestApplyAllTargetsFailureBeforeAnySwapKeepsPriorRollback: a failure on
// the first target that touched nothing (download, checksum, signature)
// leaves every prior backup in place, so the prior rollback stays offered.
func TestApplyAllTargetsFailureBeforeAnySwapKeepsPriorRollback(t *testing.T) {
	layout := threeTargetLayout(t)
	withPriorRollback(t, layout)
	boom := errors.New("simulated signature failure")

	err := applyAllTargets(layout, "1.0.0", func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the target failure", err)
	}
	assertAllAtV1(t, layout)
	if v, ok := RollbackInfo(layout); !ok || v != "0.9.0" {
		t.Errorf("RollbackInfo() = (%q, %v), want the untouched 0.9.0 rollback kept", v, ok)
	}
}

// TestApplyAllTargetsFirstFailureThatConsumedABackupWithdrawsRollback:
// go-selfupdate removes the old ".previous" just before moving the current
// binary aside, so a first-target failure from there on has broken the
// prior rollback even though nothing was swapped.
func TestApplyAllTargetsFirstFailureThatConsumedABackupWithdrawsRollback(t *testing.T) {
	layout := threeTargetLayout(t)
	withPriorRollback(t, layout)
	boom := errors.New("simulated rename failure")

	err := applyAllTargets(layout, "1.0.0", func(target string) error {
		if err := os.Remove(target + rollbackSuffix); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the target failure", err)
	}
	assertAllAtV1(t, layout)
	if v, ok := RollbackInfo(layout); ok {
		t.Errorf("RollbackInfo() = (%q, true), want the broken 0.9.0 rollback withdrawn", v)
	}
}

// TestApplyAllTargetsPutsBackATargetLeftMissing covers go-selfupdate's
// failed rollback: the current binary was moved aside to ".previous", the
// new one never took its place, and moving it back failed too.
func TestApplyAllTargetsPutsBackATargetLeftMissing(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		layout := threeTargetLayout(t)
		targets := layout.orderedTargets()
		boom := errors.New("simulated rename failure")

		err := applyAllTargets(layout, "1.0.0", func(target string) error {
			if target != targets[failAt] {
				return fakeSwap(target, "v2")
			}
			if err := os.Rename(target, target+rollbackSuffix); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("failAt=%d: err = %v, want the target failure", failAt, err)
		}
		assertAllAtV1(t, layout)
		if v, ok := RollbackInfo(layout); ok {
			t.Errorf("failAt=%d: RollbackInfo() = (%q, true), want no rollback advertised", failAt, v)
		}
	}
}

// TestApplyAllTargetsRestoreFailureKeepsBackup: a target that can't be
// restored during the unwind is reported alongside the original failure,
// and its ".previous" is left for manual recovery.
func TestApplyAllTargetsRestoreFailureKeepsBackup(t *testing.T) {
	layout := threeTargetLayout(t)
	targets := layout.orderedTargets()
	boom := errors.New("simulated download failure")

	err := applyAllTargets(layout, "1.0.0", func(target string) error {
		if target != targets[1] {
			return fakeSwap(target, "v2")
		}
		// Make targets[0]'s backup unrestorable.
		if err := os.Remove(targets[0] + rollbackSuffix); err != nil {
			return err
		}
		if err := os.Mkdir(targets[0]+rollbackSuffix, 0o755); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the target failure", err)
	}
	if !strings.Contains(err.Error(), "restore "+targets[0]) {
		t.Errorf("err = %v, want the restore failure for %s joined in", err, filepath.Base(targets[0]))
	}
	if fi, err := os.Stat(targets[0] + rollbackSuffix); err != nil || !fi.IsDir() {
		t.Errorf("unrestorable backup removed: stat = %v, %v", fi, err)
	}
	if v, ok := RollbackInfo(layout); ok {
		t.Errorf("RollbackInfo() = (%q, true), want no rollback advertised", v)
	}
}
