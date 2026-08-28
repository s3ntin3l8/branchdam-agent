package selfupdate

import (
	"fmt"
	"os"
	"strings"

	suUpdate "github.com/creativeprojects/go-selfupdate/update"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

// rollbackSuffix names the backup file each target's OldSavePath (see
// Apply's per-target su.Updater construction) saves the just-replaced
// binary to -- ".previous" rather than go-selfupdate's own default
// ".old" naming (which Apply never lets happen, since it always sets
// OldSavePath explicitly), so the file's purpose is discoverable from
// its name alone.
const rollbackSuffix = ".previous"

// rollbackVersionSuffix names the sidecar file, next to layout.Primary's
// own .previous backup, recording which version .previous actually is.
// One per InstallLayout, not per target: a single Apply call brings
// every target to the same new version atomically, so every target's
// .previous in a layout corresponds to the same previous version.
const rollbackVersionSuffix = ".previous.version"

// HasRollback reports whether layout has a previous version to roll back
// to -- both layout.Primary's .previous backup and its version sidecar
// must exist. false is the normal, expected state before any Apply has
// ever succeeded, or after a Rollback has already consumed the backup --
// not an error condition, so callers (the tray menu, `update -rollback`)
// use this to decide whether to offer the affordance at all rather than
// attempt it and handle failure.
func HasRollback(layout InstallLayout) bool {
	if _, err := os.Stat(layout.Primary + rollbackSuffix); err != nil {
		return false
	}
	_, err := PreviousVersion(layout)
	return err == nil
}

// PreviousVersion returns the version string layout would roll back to,
// or an error if no rollback is available -- what the tray menu/CLI show
// as "Roll back to vX.Y.Z" instead of a version-less generic label.
func PreviousVersion(layout InstallLayout) (string, error) {
	data, err := os.ReadFile(layout.Primary + rollbackVersionSuffix)
	if err != nil {
		return "", fmt.Errorf("selfupdate: no rollback available: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Rollback restores every target in layout from its own .previous backup
// (written by a prior Apply's per-target OldSavePath), in the SAME order
// Apply itself uses -- siblings first, layout.Primary (the running exe)
// last -- mirroring Apply's own ordering discipline for the identical
// reason: a failure on a sibling rollback aborts before the running
// binary is touched, rather than leaving the pair at different versions.
// Every target's directory is checked for writability before anything is
// touched, same as Apply. When layout.InfoPlist is set, it is rewritten
// to the rolled-back version afterward, exactly as Apply does for the
// version it applies -- go-selfupdate only ever replaces a bundle's inner
// binary, so nothing else keeps CFBundleVersion in sync.
//
// On success, every target's .previous backup and the version sidecar
// are removed (best-effort): once rolled back, that state has been fully
// consumed, and leaving it in place would let HasRollback keep advertising
// a rollback to the exact version the binary is already running.
func Rollback(layout InstallLayout) (string, error) {
	prevVersion, err := PreviousVersion(layout)
	if err != nil {
		return "", err
	}

	for _, dir := range layout.targetDirs() {
		if err := checkWritable(dir); err != nil {
			return "", err
		}
	}

	for _, target := range layout.orderedTargets() {
		if err := restoreOne(target); err != nil {
			return "", fmt.Errorf("selfupdate: rollback %s: %w", target, err)
		}
	}

	for _, target := range layout.orderedTargets() {
		_ = os.Remove(target + rollbackSuffix)
	}
	_ = os.Remove(layout.Primary + rollbackVersionSuffix)

	if layout.InfoPlist != "" {
		plist := appbundle.RenderInfoPlist(prevVersion)
		if err := os.WriteFile(layout.InfoPlist, []byte(plist), 0o644); err != nil {
			return "", fmt.Errorf("selfupdate: rollback: update %s: %w", layout.InfoPlist, err)
		}
	}

	return prevVersion, nil
}

// restoreOne swaps target's current content for its own .previous backup
// via go-selfupdate's exported update.Apply -- the same safe
// create-new/rename-aside/rename-into-place mechanics Apply itself relies
// on for a running executable, reused here for the reverse direction. No
// OldSavePath is set on this call: the (bad) binary being replaced is
// simply discarded, matching an operator's intent when rolling back.
func restoreOne(target string) error {
	f, err := os.Open(target + rollbackSuffix)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	return suUpdate.Apply(f, suUpdate.Options{TargetPath: target})
}
