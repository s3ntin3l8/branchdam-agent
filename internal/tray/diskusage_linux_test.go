//go:build linux

package tray

import "testing"

// TestDiskUsageAgainstRealTempDir is the Linux half of issue #274's
// disk-usage tracking -- verified once by hand during development against
// `df -B1` on this same host (Bsize/Frsize agreed on every local
// filesystem tried), so this permanent test checks the structural
// properties a manual df comparison can't run unattended in CI: no error,
// a nonzero total, and free never exceeding it.
func TestDiskUsageAgainstRealTempDir(t *testing.T) {
	dir := t.TempDir()

	free, total, err := diskUsage(dir)
	if err != nil {
		t.Fatalf("diskUsage(%q) error = %v", dir, err)
	}
	if total == 0 {
		t.Error("total = 0, want a real filesystem size")
	}
	if free > total {
		t.Errorf("free = %d, total = %d -- free must never exceed total", free, total)
	}
}

func TestDiskUsageMissingPathReturnsPathError(t *testing.T) {
	_, _, err := diskUsage("/nonexistent/path/for/disk/usage/test")
	if err == nil {
		t.Fatal("expected an error for a nonexistent path, got nil")
	}
}
