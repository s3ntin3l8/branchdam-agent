package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

// Windows sibling names are hardcoded, never derived by munging execPath's
// basename -- go-selfupdate's DecompressCommand fails outright for a name
// that isn't actually present in the archive, which would abort the whole
// apply for a renamed exe. winUIExe is appbundle.WinUIBinaryName, not a
// fourth independent literal -- see that constant's own doc comment for
// why internal/tray needs the identical name reachable without importing
// this package.
const (
	winConsoleExe = "branchdam-agent.exe"
	winTrayExe    = "branchdam-agent-tray.exe"
	winUIExe      = appbundle.WinUIBinaryName
)

// winKnownExes is every Windows binary shipped from the same release
// archive (branchdam-agent-windows-amd64.zip). windowsSiblings only
// proceeds for a resolved path whose basename is one of these -- an
// unknown basename (a renamed exe, a dev build) gets no siblings at all.
var winKnownExes = []string{winConsoleExe, winTrayExe, winUIExe}

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
	// BundlePath is a pure path-shape check (does resolved sit at
	// .../Foo.app/Contents/MacOS/binary?), not gated on runtime.GOOS, so
	// it stays exercised by Linux CI rather than only ever running on a
	// darwin host.
	if bundle := BundlePath(resolved); bundle != "" {
		layout.InfoPlist = filepath.Join(bundle, "Contents", "Info.plist")
		if sibling := macOSUISibling(resolved); sibling != "" {
			layout.Siblings = append(layout.Siblings, sibling)
		}
	}

	return layout, nil
}

// windowsSiblings returns every other winKnownExes entry actually present
// beside resolved, so a self-update from any one of the three Windows
// binaries carries the other two along in the same Apply call -- see
// InstallLayout's own doc comment for why siblings are replaced before
// Primary. An entry that isn't on disk (e.g. an install predating the UI
// binary shipping) is silently skipped rather than failing the whole
// update: go-selfupdate's DecompressCommand only needs to find what it's
// asked for, and a target that doesn't exist yet isn't asked for.
func windowsSiblings(resolved string) []string {
	base := filepath.Base(resolved)
	if !slices.Contains(winKnownExes, base) {
		return nil
	}
	dir := filepath.Dir(resolved)
	var siblings []string
	for _, name := range winKnownExes {
		if name == base {
			continue
		}
		siblingPath := filepath.Join(dir, name)
		if _, err := os.Stat(siblingPath); err != nil {
			continue
		}
		siblings = append(siblings, siblingPath)
	}
	return siblings
}

// macOSUISibling returns the UI binary's path next to resolved inside the
// same Contents/MacOS/ directory, or "" if it isn't there (an install
// predating the UI binary shipping, or a build without it). Unlike
// windowsSiblings, there is only ever one other binary to check on macOS --
// the bundle has exactly one CFBundleExecutable (the tray) plus the UI
// binary, no third target. This is a fact about the bundle shape, not an
// enforced invariant: cmd/branchdam-agent-ui has no self-update code path
// today, so resolved is always the tray binary here in practice, but
// nothing in this package would misbehave if that changed -- BundlePath's
// own .../Contents/MacOS/ check would still resolve correctly regardless of
// which of the two binaries called DetectLayout.
func macOSUISibling(resolved string) string {
	siblingPath := filepath.Join(filepath.Dir(resolved), appbundle.UIBinaryName)
	if _, err := os.Stat(siblingPath); err != nil {
		return ""
	}
	return siblingPath
}

// BundlePath returns the enclosing .app bundle's path when execPath is
// .../Foo.app/Contents/MacOS/binary, else "". Exported so
// cmd/branchdam-agent's relaunch step can detect a bundled tray and
// restart it via "open -a" instead of exec'ing the inner binary directly
// -- see that package's relaunchSelf.
func BundlePath(execPath string) string {
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
	return appDir
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
	defer func() { _ = os.Remove(name) }()

	// The probe file is empty, so a Close failure here can't lose
	// buffered data the way it could for a real write -- but it can
	// still mean the filesystem rejected the write in a way CreateTemp's
	// own open didn't catch (e.g. a quota hit), so it's still surfaced
	// rather than silently discarded.
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrTargetNotWritable, dir, err)
	}
	return nil
}
