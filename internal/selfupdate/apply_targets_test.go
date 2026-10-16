package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
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

// TestApplyAllTargetsClearsStaleSidecarBeforeSwapping covers a prior
// Apply's rollback: its sidecar must not survive a failed apply that has
// already overwritten some of that rollback's backups.
func TestApplyAllTargetsClearsStaleSidecarBeforeSwapping(t *testing.T) {
	layout := threeTargetLayout(t)
	for _, target := range layout.orderedTargets() {
		if err := os.WriteFile(target+rollbackSuffix, []byte("v0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(layout.Primary+rollbackVersionSuffix, []byte("0.9.0"), 0o600); err != nil {
		t.Fatal(err)
	}

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
