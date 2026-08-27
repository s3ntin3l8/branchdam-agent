package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	if layout.Primary != bin {
		t.Errorf("Primary = %q, want %q", layout.Primary, bin)
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
	want := filepath.Join(dir, "branchdam-agent.app", "Contents", "Info.plist")
	if layout.InfoPlist != want {
		t.Errorf("InfoPlist = %q, want %q", layout.InfoPlist, want)
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

	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't apply")
	}
	err := checkWritable(roDir)
	if !errors.Is(err, ErrTargetNotWritable) {
		t.Errorf("got err %v, want ErrTargetNotWritable", err)
	}
}
