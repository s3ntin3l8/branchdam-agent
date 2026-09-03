package tray

import (
	"context"
	"path/filepath"
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

	cleaned := filepath.Clean(path)
	for _, dir := range r.WatchDirs() {
		if filepath.Clean(dir) == cleaned {
			if notify != nil {
				notify(ctx, "Already ingesting this path")
			}
			return false, IngestSummary{}
		}
	}

	return true, r.TriggerIngest(ctx, path)
}
