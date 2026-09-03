package tray

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
)

// handleImportFolder executes the "Import from folder…" manual source selection
// flow (issue #80): prompts the operator for a directory via pickDir, checks
// whether the chosen directory is already in r.WatchDirs(), shows an OS
// notification via notify if so and aborts, or triggers an ingest of the path
// directly as a one-shot source via r.TriggerIngest.
//
// Returns true iff r.TriggerIngest was actually called (either success or
// engine error -- both dispatch IngestSummary through Runner.SetLastIngest
// and the existing drainSkipped / pruneSkipped / lastIngest rendering paths
// already surface it; no second return value is needed at this layer).
func handleImportFolder(ctx context.Context, r *Runner, pickDir func(ctx context.Context) (string, error), notify func(ctx context.Context, msg string)) bool {
	if pickDir == nil || r == nil {
		return false
	}

	path, err := pickDir(ctx)
	if err != nil || path == "" {
		return false
	}

	for _, dir := range r.WatchDirs() {
		if samePath(dir, path) {
			if notify != nil {
				notify(ctx, "Already ingesting this path")
			}
			return false
		}
	}

	r.TriggerIngest(ctx, path)
	return true
}

// samePath reports whether two paths refer to the same directory,
// normalizing separators, case (on Windows and macOS -- the tray's
// two supported platforms), and symlinks (so /var/folders/X on macOS
// matches the same physical directory via /private/var/folders/X, and
// /proc/<pid>/root/x on Linux matches the canonical /x).
func samePath(a, b string) bool {
	return samePathOS(a, b, runtime.GOOS)
}

func samePathOS(a, b, goos string) bool {
	ca := filepath.Clean(a)
	cb := filepath.Clean(b)
	if ca == cb {
		return true
	}
	if goos == "windows" || goos == "darwin" {
		if strings.EqualFold(ca, cb) {
			return true
		}
	}
	absA, errA := filepath.Abs(ca)
	absB, errB := filepath.Abs(cb)
	if errA == nil && errB == nil {
		if absA == absB {
			return true
		}
		if goos == "windows" || goos == "darwin" {
			if strings.EqualFold(absA, absB) {
				return true
			}
		}
		// Symlink resolution: a watched path can be reached through a
		// different lexical path on macOS (/var is a symlink to
		// /private/var) or Linux (/proc/<pid>/root points at /).
		// Without this, picking the same physical directory via the
		// alternative path skips the "Already ingesting this path"
		// notification and triggers a redundant concurrent ingest.
		// EvalSymlinks fails if either side is missing (e.g. an ejected
		// card root), so it falls through to the lexical comparison
		// already done above.
		linkA, errLA := filepath.EvalSymlinks(absA)
		linkB, errLB := filepath.EvalSymlinks(absB)
		if errLA == nil && errLB == nil {
			if linkA == linkB {
				return true
			}
			if goos == "windows" || goos == "darwin" {
				if strings.EqualFold(linkA, linkB) {
					return true
				}
			}
		}
	}
	return false
}
