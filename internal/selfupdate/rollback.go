package selfupdate

import (
	"errors"
	"fmt"
	"log/slog"
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
// to. See RollbackInfo for a version that also returns the version
// string in the same call -- prefer that on a hot path (the tray's
// menu-refresh tick) where both are needed, to avoid statting the backup
// and reading the sidecar twice.
func HasRollback(layout InstallLayout) bool {
	_, ok := RollbackInfo(layout)
	return ok
}

// RollbackInfo reports whether layout has a previous version to roll
// back to, and if so, what it is -- both layout.Primary's .previous
// backup and its version sidecar must exist. ok=false is the normal,
// expected state before any Apply has ever succeeded, or after a
// Rollback has already consumed the backup -- not an error condition, so
// callers (the tray menu, `update -rollback`) use this to decide whether
// to offer the affordance at all rather than attempt it and handle
// failure.
func RollbackInfo(layout InstallLayout) (version string, ok bool) {
	if _, err := os.Stat(layout.Primary + rollbackSuffix); err != nil {
		return "", false
	}
	v, err := PreviousVersion(layout)
	if err != nil {
		return "", false
	}
	return v, true
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
// touched, same as Apply.
//
// Each target's own .previous backup is removed immediately after that
// target's restore succeeds, not batched at the end -- a Hermes review
// finding on this PR caught that batching left every backup (including
// ones already fully consumed) in place after a mid-loop failure,
// advertising "Roll back to vX" for a state that was actually only
// partially rolled back. restoreOne treats an already-missing backup as
// a no-op success rather than an error specifically so a retry after
// such a failure can re-run this same loop safely: whichever targets
// already succeeded are skipped, and whichever didn't are retried.
//
// When layout.InfoPlist is set, it is rewritten to the rolled-back
// version once every target has been restored, exactly as Apply does for
// the version it applies. This happens BEFORE the version sidecar is
// removed (the loop's own per-target backup removal already happened by
// this point) -- another Hermes finding: if the plist write itself
// fails, the sidecar staying in place means PreviousVersion/HasRollback
// still work on a retry, even though every binary is already correctly
// rolled back by then.
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
		// Best-effort: the restore itself already succeeded, so a
		// failure here doesn't put the binary in an inconsistent state
		// -- it just means this target's backup outlives its usefulness
		// (RollbackInfo would then see a stale backup for a version
		// that's now already live). Logged, not swallowed silently, so
		// it's diagnosable rather than a Hermes review finding waiting
		// to happen again.
		if err := os.Remove(target + rollbackSuffix); err != nil && !os.IsNotExist(err) {
			slog.Warn("selfupdate: rollback: could not remove consumed backup", "path", target+rollbackSuffix, "err", err)
		}
	}

	if layout.InfoPlist != "" {
		plist := appbundle.RenderInfoPlist(prevVersion)
		if err := os.WriteFile(layout.InfoPlist, []byte(plist), 0o644); err != nil {
			return "", fmt.Errorf("selfupdate: rollback: update %s: %w", layout.InfoPlist, err)
		}
	}

	// Best-effort, same reasoning as the per-target backup removal above:
	// the rollback itself already fully succeeded by this point.
	if err := os.Remove(layout.Primary + rollbackVersionSuffix); err != nil && !os.IsNotExist(err) {
		slog.Warn("selfupdate: rollback: could not remove version sidecar", "path", layout.Primary+rollbackVersionSuffix, "err", err)
	}

	return prevVersion, nil
}

// restoreOne swaps target's current content for its own .previous backup
// via go-selfupdate's exported update.Apply -- the same safe
// create-new/rename-aside/rename-into-place mechanics Apply itself relies
// on for a running executable, reused here for the reverse direction.
// A missing backup is treated as an already-completed no-op (see
// Rollback's own doc comment on why that makes a retry after a
// mid-rollback failure safe), not an error.
//
// discardPath is used as this call's OWN OldSavePath rather than leaving
// it unset: go-selfupdate's default behavior for an unset OldSavePath is
// to try removing the binary being replaced (the bad version rollback is
// discarding) and, on Windows, fall back to hiding it as a
// ".<basename>.old" file when that remove fails (a running exe can't be
// deleted) -- a stray artifact a Hermes review suggestion on this PR
// flagged. Giving the swap an explicit, throwaway OldSavePath and
// removing it ourselves right after avoids depending on that
// hide-on-Windows fallback at all.
func restoreOne(target string) error {
	backupPath := target + rollbackSuffix
	f, err := os.Open(backupPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open backup: %w", err)
	}
	defer func() { _ = f.Close() }()

	discardPath := target + ".rollback-discard"
	if err := suUpdate.Apply(f, suUpdate.Options{TargetPath: target, OldSavePath: discardPath}); err != nil {
		return err
	}
	// Best-effort, same reasoning and logging as Rollback's own cleanup
	// removes: the swap itself already succeeded, so a failure here just
	// leaves a throwaway discarded-binary file behind, not an
	// inconsistent state.
	if err := os.Remove(discardPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("selfupdate: rollback: could not remove discarded binary", "path", discardPath, "err", err)
	}
	return nil
}
