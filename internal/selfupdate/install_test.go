package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

func TestDetectLayoutPlain(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "branchdam-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := DetectLayout(bin)
	if err != nil {
		t.Fatal(err)
	}
	wantPrimary, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Primary != wantPrimary {
		t.Errorf("Primary = %q, want %q", layout.Primary, wantPrimary)
	}
	if len(layout.Siblings) != 0 {
		t.Errorf("Siblings = %v, want none", layout.Siblings)
	}
	if layout.InfoPlist != "" {
		t.Errorf("InfoPlist = %q, want empty", layout.InfoPlist)
	}
}

func TestDetectLayoutMacBundle(t *testing.T) {
	dir := t.TempDir()
	macOSDir := filepath.Join(dir, "branchdam-agent.app", "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(macOSDir, "branchdam-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := DetectLayout(bin)
	if err != nil {
		t.Fatal(err)
	}
	resolvedBin, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(BundlePath(resolvedBin), "Contents", "Info.plist")
	if layout.InfoPlist != want {
		t.Errorf("InfoPlist = %q, want %q", layout.InfoPlist, want)
	}
	if len(layout.Siblings) != 0 {
		t.Errorf("Siblings = %v, want none (no UI binary present in this bundle)", layout.Siblings)
	}
}

// TestDetectLayoutMacBundleWithUIBinary confirms the UI binary graduates
// to a Sibling when it's actually present next to the tray inside
// Contents/MacOS/ -- the packaging PR's whole point is that both binaries
// self-update together.
func TestDetectLayoutMacBundleWithUIBinary(t *testing.T) {
	dir := t.TempDir()
	macOSDir := filepath.Join(dir, "branchdam-agent.app", "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(macOSDir, "branchdam-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	uiBin := filepath.Join(macOSDir, appbundle.UIBinaryName)
	if err := os.WriteFile(uiBin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	layout, err := DetectLayout(bin)
	if err != nil {
		t.Fatal(err)
	}
	resolvedUIBin, err := filepath.EvalSymlinks(uiBin)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Siblings) != 1 || layout.Siblings[0] != resolvedUIBin {
		t.Errorf("Siblings = %v, want [%q]", layout.Siblings, resolvedUIBin)
	}
}

func TestDetectLayoutTranslocated(t *testing.T) {
	// filepath.EvalSymlinks requires the path to exist, so build a real
	// (if fake) translocation-shaped tree under TempDir rather than
	// asserting against a literal /private/var/folders path.
	dir := t.TempDir()
	translocated := filepath.Join(dir, "AppTranslocation", "abc123", "d", "branchdam-agent.app", "Contents", "MacOS")
	if err := os.MkdirAll(translocated, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(translocated, "branchdam-agent")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := DetectLayout(bin)
	if !errors.Is(err, ErrTranslocated) {
		t.Errorf("got err %v, want ErrTranslocated", err)
	}
}

// TestWindowsSiblings exercises windowsSiblings directly -- it's pure
// (basename checks + os.Stat, no runtime.GOOS branch of its own), so
// unlike DetectLayout's own windows-gated call site, it's fully testable
// on every host, including this repo's own Linux CI.
func TestWindowsSiblings(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("unknown basename gets no siblings", func(t *testing.T) {
		unknown := write("not-a-branchdam-binary.exe")
		if got := windowsSiblings(unknown); got != nil {
			t.Errorf("windowsSiblings(%q) = %v, want nil", unknown, got)
		}
	})

	t.Run("missing siblings are silently skipped", func(t *testing.T) {
		soloDir := t.TempDir()
		solo := filepath.Join(soloDir, winConsoleExe)
		if err := os.WriteFile(solo, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := windowsSiblings(solo); got != nil {
			t.Errorf("windowsSiblings(%q) = %v, want nil (no other exe present)", solo, got)
		}
	})

	t.Run("all three present, all returned except self", func(t *testing.T) {
		console := write(winConsoleExe)
		tray := write(winTrayExe)
		ui := write(winUIExe)

		got := windowsSiblings(console)
		want := []string{tray, ui}
		if len(got) != len(want) || !slices.Contains(got, want[0]) || !slices.Contains(got, want[1]) {
			t.Errorf("windowsSiblings(%q) = %v, want %v (order-independent)", console, got, want)
		}
		if slices.Contains(got, console) {
			t.Errorf("windowsSiblings(%q) included itself: %v", console, got)
		}
	})
}

func TestOrderedTargetsSiblingsFirst(t *testing.T) {
	l := InstallLayout{Primary: "primary", Siblings: []string{"sibling"}}
	got := l.orderedTargets()
	want := []string{"sibling", "primary"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("orderedTargets() = %v, want %v", got, want)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Errorf("checkWritable on a fresh temp dir: %v", err)
	}

	roDir := filepath.Join(dir, "ro")
	if err := os.Mkdir(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not make a directory read-only on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't apply")
	}
	err := checkWritable(roDir)
	if !errors.Is(err, ErrTargetNotWritable) {
		t.Errorf("got err %v, want ErrTargetNotWritable", err)
	}
}
