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
func handleImportFolder(ctx context.Context, r *Runner, pickDir func(ctx context.Context) (string, error), notify func(ctx context.Context, msg string)) (ingested bool, summary IngestSummary) {
	if pickDir == nil || r == nil {
		return false, IngestSummary{}
	}

	path, err := pickDir(ctx)
	if err != nil || path == "" {
		return false, IngestSummary{}
	}

	for _, dir := range r.WatchDirs() {
		if samePath(dir, path) {
			if notify != nil {
				notify(ctx, "Already ingesting this path")
			}
			return false, IngestSummary{}
		}
	}

	return true, r.TriggerIngest(ctx, path)
}

// samePath reports whether two paths refer to the same directory path,
// normalizing separators and accounting for case-insensitive filesystems
// on Windows and macOS (the tray's supported platforms).
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
	}
	return false
}
