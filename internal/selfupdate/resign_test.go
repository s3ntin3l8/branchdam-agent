package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// withResignAppBundle swaps the package-level resignAppBundle var for fn
// and restores the real implementation on test cleanup -- the seam
// resignAppBundle's own doc comment describes, mirroring sigstore.go's
// fetchClient var.
func withResignAppBundle(t *testing.T, fn func(bundleDir string) error) {
	t.Helper()
	orig := resignAppBundle
	resignAppBundle = fn
	t.Cleanup(func() { resignAppBundle = orig })
}

func TestDefaultResignAppBundleNoOpsOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test asserts the off-darwin no-op path; darwin exercises the real codesign call instead")
	}
	// No codesign binary exists on this GOOS -- if defaultResignAppBundle
	// didn't gate on runtime.GOOS, this would fail with an exec error
	// instead of returning nil.
	if err := defaultResignAppBundle(filepath.Join(t.TempDir(), "branchdam-agent.app")); err != nil {
		t.Errorf("defaultResignAppBundle on %s = %v, want nil (no-op off darwin)", runtime.GOOS, err)
	}
}

func TestRollbackCallsResignAppBundleWithBundleDir(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "branchdam-agent.app")
	primary := filepath.Join(bundleDir, "Contents", "MacOS", "branchdam-agent")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAppliedState(t, primary, "old binary bytes", "2.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(bundleDir, "Contents", "Info.plist")
	if err := os.WriteFile(plistPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, InfoPlist: plistPath}

	var gotDir string
	var calls int
	withResignAppBundle(t, func(d string) error {
		calls++
		gotDir = d
		return nil
	})

	if _, err := Rollback(layout); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if calls != 1 {
		t.Errorf("resignAppBundle called %d times, want 1", calls)
	}
	if gotDir != bundleDir {
		t.Errorf("resignAppBundle called with %q, want %q", gotDir, bundleDir)
	}
}

func TestRollbackPropagatesResignError(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent.app", "Contents", "MacOS", "branchdam-agent")
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAppliedState(t, primary, "old binary bytes", "2.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(dir, "branchdam-agent.app", "Contents", "Info.plist")
	if err := os.WriteFile(plistPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary, InfoPlist: plistPath}

	wantErr := errors.New("codesign: boom")
	withResignAppBundle(t, func(string) error { return wantErr })

	_, err := Rollback(layout)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("Rollback error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestRollbackSkipsResignWithoutInfoPlist(t *testing.T) {
	// A non-macOS InstallLayout (no InfoPlist set at all) must never call
	// resignAppBundle -- it's not a bundle, there's nothing to re-sign.
	dir := t.TempDir()
	primary := filepath.Join(dir, "branchdam-agent")
	writeAppliedState(t, primary, "old binary bytes", "2.0.0")
	if err := os.WriteFile(primary+rollbackVersionSuffix, []byte("2.0.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout := InstallLayout{Primary: primary}

	var calls int
	withResignAppBundle(t, func(string) error {
		calls++
		return nil
	})

	if _, err := Rollback(layout); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if calls != 0 {
		t.Errorf("resignAppBundle called %d times for a non-bundle layout, want 0", calls)
	}
}
