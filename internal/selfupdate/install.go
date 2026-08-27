package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Windows sibling names are hardcoded, never derived by munging execPath's
// basename -- go-selfupdate's DecompressCommand fails outright for a name
// that isn't actually present in the archive, which would abort the whole
// apply for a renamed exe.
const (
	winConsoleExe = "branchdam-agent.exe"
	winTrayExe    = "branchdam-agent-tray.exe"
)

// InstallLayout is every file one Apply call touches, derived from the
// running executable's path. Siblings are applied before Primary: on a
// sibling, go-selfupdate's cleanup (removing the old binary) succeeds
// immediately since nothing has it open, so a failure there aborts before
// the running binary is touched, leaving both at the old version rather
// than a version-skewed pair.
type InstallLayout struct {
	// Primary is the running binary; always replaced last.
	Primary string
	// Siblings are other binaries from the same release archive beside
	// Primary (the other Windows .exe); replaced first.
	Siblings []string
	// InfoPlist is set only when Primary is inside a macOS .app bundle
	// (.../Contents/MacOS/binary) -- go-selfupdate replaces only the
	// inner binary, so Apply rewrites this file locally afterward to
	// keep CFBundleVersion in sync.
	InfoPlist string
}

// orderedTargets returns every binary path to replace, siblings first,
// Primary last.
func (l InstallLayout) orderedTargets() []string {
	return append(append([]string{}, l.Siblings...), l.Primary)
}

// targetDirs returns the distinct, sorted set of directories orderedTargets
// live in -- what Apply checks for writability before downloading anything.
func (l InstallLayout) targetDirs() []string {
	seen := make(map[string]struct{})
	for _, t := range l.orderedTargets() {
		seen[filepath.Dir(t)] = struct{}{}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// DetectLayout resolves execPath (through filepath.EvalSymlinks --
// go-selfupdate's UpdateTo, unlike its UpdateCommand helper, does not do
// this resolution itself) and enumerates what one Apply call should
// replace. Returns ErrTranslocated for a macOS App-Translocation path.
func DetectLayout(execPath string) (InstallLayout, error) {
	resolved, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return InstallLayout{}, fmt.Errorf("selfupdate: resolve executable path %q: %w", execPath, err)
	}

	if isTranslocated(resolved) {
		return InstallLayout{}, fmt.Errorf("%w: %s", ErrTranslocated, resolved)
	}

	layout := InstallLayout{Primary: resolved}

	if runtime.GOOS == "windows" {
		layout.Siblings = windowsSiblings(resolved)
	}
	// macAppInfoPlist is a pure path-shape check (does resolved sit at
	// .../Foo.app/Contents/MacOS/binary?), not gated on runtime.GOOS, so
	// it stays exercised by Linux CI rather than only ever running on a
	// darwin host.
	layout.InfoPlist = macAppInfoPlist(resolved)

	return layout, nil
}

func windowsSiblings(resolved string) []string {
	dir := filepath.Dir(resolved)
	var sibling string
	switch filepath.Base(resolved) {
	case winConsoleExe:
		sibling = winTrayExe
	case winTrayExe:
		sibling = winConsoleExe
	default:
		return nil
	}
	siblingPath := filepath.Join(dir, sibling)
	if _, err := os.Stat(siblingPath); err != nil {
		return nil
	}
	return []string{siblingPath}
}

// macAppInfoPlist returns the enclosing bundle's Info.plist path when
// execPath is .../Foo.app/Contents/MacOS/binary, else "".
func macAppInfoPlist(execPath string) string {
	macOS := filepath.Dir(execPath)
	if filepath.Base(macOS) != "MacOS" {
		return ""
	}
	contents := filepath.Dir(macOS)
	if filepath.Base(contents) != "Contents" {
		return ""
	}
	appDir := filepath.Dir(contents)
	if !strings.HasSuffix(appDir, ".app") {
		return ""
	}
	return filepath.Join(contents, "Info.plist")
}

func isTranslocated(path string) bool {
	return strings.Contains(path, string(filepath.Separator)+"AppTranslocation"+string(filepath.Separator))
}

// checkWritable probes dir for write access by creating and removing a
// temp file -- cheaper and more accurate than inspecting permission bits,
// which don't account for ACLs, filesystem-level read-only mounts, or
// (on Windows) UAC virtualization quirks.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".branchdam-agent-update-*")
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrTargetNotWritable, dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}
