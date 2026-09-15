package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestUpdateBundleInfoPlistWritesVersionAndResigns and
// TestUpdateBundleInfoPlistNonFatalOnResignFailure test the shared helper
// Apply and Rollback both call directly, rather than through either
// caller's own machinery. This is the direct answer to a Hermes review
// suggestion that Apply's call site had no test mirroring Rollback's three
// (TestRollbackCallsResignAppBundleWithBundleDir,
// TestRollbackSucceedsDespiteResignFailure,
// TestRollbackSkipsResignWithoutInfoPlist below): after this refactor,
// Apply's call site is exactly `updateBundleInfoPlist(layout.InfoPlist,
// release.Version())` -- identical in shape to Rollback's own call, one
// line, trivially reviewable -- so testing the shared function once here
// covers both callers' behavior rather than duplicating a second
// Rollback-shaped integration test under Apply's name. A true Apply
// happy-path integration test remains out of proportion regardless: it
// would need a fake go-selfupdate Source (see that package's Source
// interface) plus faking this package's own fetchClient (sigstore.go) to
// avoid real network calls for the Sigstore attestation preflight, before
// ever reaching this code.
func TestUpdateBundleInfoPlistWritesVersionAndResigns(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "branchdam-agent.app")
	plistPath := filepath.Join(bundleDir, "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotDir string
	var calls int
	withResignAppBundle(t, func(d string) error {
		calls++
		gotDir = d
		return nil
	})

	if err := updateBundleInfoPlist(plistPath, "3.1.4"); err != nil {
		t.Fatalf("updateBundleInfoPlist: %v", err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "3.1.4") {
		t.Errorf("Info.plist = %q, want it to mention version 3.1.4", got)
	}
	if calls != 1 {
		t.Errorf("resignAppBundle called %d times, want 1", calls)
	}
	if gotDir != bundleDir {
		t.Errorf("resignAppBundle called with %q, want %q", gotDir, bundleDir)
	}
}

func TestUpdateBundleInfoPlistNonFatalOnResignFailure(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "branchdam-agent.app")
	plistPath := filepath.Join(bundleDir, "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}

	withResignAppBundle(t, func(string) error { return errors.New("codesign: boom") })

	if err := updateBundleInfoPlist(plistPath, "3.1.4"); err != nil {
		t.Fatalf("updateBundleInfoPlist returned an error despite a successful plist write: %v", err)
	}
	got, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "3.1.4") {
		t.Errorf("Info.plist = %q, want it to mention version 3.1.4 even though resign failed", got)
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

// TestRollbackSucceedsDespiteResignFailure pins the corrected, non-fatal
// contract (a Hermes-reviewer-caught regression against an earlier version
// of this change, which made resign failure fatal): a resign failure must
// never fail Rollback. By the point resign runs, every target has already
// been restored and its .previous backup already consumed -- the rollback
// has genuinely, fully succeeded; only the signature seal is stale.
// Reporting that as a Rollback failure, with no backup left to retry from,
// would be strictly worse than the merely-stale seal. Caught for real on
// darwin CI: the pre-fix version of this code broke
// TestRollbackRewritesInfoPlist there, since that test's flat (non-bundle)
// fixture path makes the real codesign call fail every time.
func TestRollbackSucceedsDespiteResignFailure(t *testing.T) {
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

	var calls int
	withResignAppBundle(t, func(string) error {
		calls++
		return errors.New("codesign: boom")
	})

	version, err := Rollback(layout)
	if err != nil {
		t.Fatalf("Rollback returned an error despite a fully-successful restore: %v", err)
	}
	if version != "2.0.0" {
		t.Errorf("Rollback version = %q, want %q", version, "2.0.0")
	}
	if calls != 1 {
		t.Errorf("resignAppBundle called %d times, want 1 (the failure must still be attempted, just not fatal)", calls)
	}
	got, err := os.ReadFile(primary)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old binary bytes" {
		t.Errorf("primary binary after rollback = %q, want the restored old content", got)
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
