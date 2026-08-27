// Package prune implements the logic behind `branchdam-agent prune`
// (branchdam#230-adjacent -- see internal/config.PruneConfig's doc comment
// for exactly what this does and does not cover; it is NOT real Tier-1
// LOCAL_SCRATCH/NLE-cache pruning, which stays architecturally blocked).
// Extracted from cmd/branchdam-agent/prune.go as a pure move (no behavior
// change) -- cmd/branchdam-agent/prune.go keeps only flag parsing and the
// config/queue/client wiring `prune`'s CLI surface needs.
package prune

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// defaultMinAgeHours is applied when Prune.Enabled is true and
// Prune.MinAgeHours is left at its zero value -- config.Load applies no
// defaulting of its own (see IngestConfig.PollIntervalSecs' identical
// pattern), so this lives at the point of use.
const defaultMinAgeHours = 24

// nodeStatusBatchSize chunks NodeUUID lookups to stay under the server's
// own per-request cap (branchDAM's agentNodeStatusMaxUUIDs, 200).
const nodeStatusBatchSize = 200

// Stats is one pass's accounting -- printed in full (PrintReport) so a
// skip is always visible and attributable, never silently dropped.
type Stats struct {
	Evaluated                int
	Pruned                   int
	FreedBytes               int64
	SkippedSidecar           int
	SkippedAlreadyGone       int
	SkippedTooYoung          int
	SkippedNotVerified       int
	SkippedFileChanged       int
	SkippedOutsideRoot       int
	SkippedUnexpectedSymlink int
	Errors                   int
}

// Pass implements one prune pass end to end. Deliberately narrow in scope:
// only queue.db rows (offline-ingested files) are ever considered -- a
// plain online `ingest` has no durable local-path ledger to work from at
// all. Two independent safety gates run before any deletion, neither of
// which trusts the server response alone:
//
//  1. A containment check (filepath.EvalSymlinks on both LocalEditRoot and
//     the candidate path) refuses to touch anything that doesn't resolve
//     under LocalEditRoot -- this agent has no storage.Guard equivalent, so
//     this check *is* that safety net, hand-rolled.
//  2. A TOCTOU re-stat compares the file's current size/mtime against what
//     was recorded at ingest time (Record.SizeBytes/MtimeUnix), mirroring
//     branchDAM's own ErrFileChangedSincePlan. This depends on
//     ingestFileOffline's os.Chtimes call making the local copy's mtime
//     equal to the source's mtime at ingest time -- if that call is ever
//     removed, this check silently stops catching anything.
func Pass(ctx context.Context, w io.Writer, client *branchdam.Client, store *queue.Store, cfg config.Config, now time.Time, dryRun bool) (Stats, error) {
	var stats Stats

	minAgeHours := cfg.Prune.MinAgeHours
	if minAgeHours <= 0 {
		minAgeHours = defaultMinAgeHours
	}
	minAgeSecs := int64(minAgeHours) * 3600

	resolvedRoot, err := filepath.EvalSymlinks(cfg.Ingest.LocalEditRoot)
	if err != nil {
		return stats, fmt.Errorf("resolve ingest.localEditRoot %q: %w", cfg.Ingest.LocalEditRoot, err)
	}

	rows, err := store.All(ctx)
	if err != nil {
		return stats, fmt.Errorf("list queue rows: %w", err)
	}

	type candidate struct {
		rec queue.Record
	}
	var candidates []candidate

	for _, r := range rows {
		if !r.Done() {
			continue
		}
		// Sidecars (.xmp/.srt) never get their own EVENT_NODE_CREATED (see
		// ingestFileOffline's doc comment) -- their NodeUUID never
		// corresponds to a media_nodes row, so node-status would report
		// Found=false forever. Skip them rather than waste a lookup that
		// can never succeed.
		if r.Kind != queue.KindMedia {
			stats.SkippedSidecar++
			continue
		}
		stats.Evaluated++

		info, err := os.Lstat(r.LocalPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.SkippedAlreadyGone++
				continue
			}
			stats.Errors++
			_, _ = fmt.Fprintf(w, "[ERROR] stat %s: %v\n", r.LocalPath, err)
			continue
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			stats.SkippedUnexpectedSymlink++
			_, _ = fmt.Fprintf(w, "[SKIP] %s: unexpected symlink at a normal ingest path, refusing to touch\n", r.LocalPath)
			continue
		}
		if now.Unix()-r.MtimeUnix < minAgeSecs {
			stats.SkippedTooYoung++
			continue
		}
		candidates = append(candidates, candidate{rec: r})
	}

	// Batch NodeUUID lookups, chunked to the server's cap.
	statusByUUID := make(map[string]branchdam.NodeStatusEntry, len(candidates))
	for start := 0; start < len(candidates); start += nodeStatusBatchSize {
		end := start + nodeStatusBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		uuids := make([]string, 0, end-start)
		for _, c := range candidates[start:end] {
			uuids = append(uuids, c.rec.NodeUUID)
		}
		resp, err := client.NodeStatus(ctx, branchdam.NodeStatusRequest{NodeUUIDs: uuids})
		if err != nil {
			return stats, fmt.Errorf("node-status batch [%d:%d]: %w", start, end, err)
		}
		for _, s := range resp.Statuses {
			statusByUUID[s.NodeUUID] = s
		}
	}

	for _, c := range candidates {
		s, ok := statusByUUID[c.rec.NodeUUID]
		eligible := ok && s.Found && s.Verified && s.Tier == "TIER3_MASTER_ARCHIVE" &&
			(s.LifecycleState == "ACTIVE" || s.LifecycleState == "HIDDEN")
		if !eligible {
			stats.SkippedNotVerified++
			continue
		}

		within, err := withinRoot(resolvedRoot, c.rec.LocalPath)
		if err != nil || !within {
			stats.SkippedOutsideRoot++
			_, _ = fmt.Fprintf(w, "[SKIP] %s: does not resolve under ingest.localEditRoot %q, refusing to delete\n", c.rec.LocalPath, cfg.Ingest.LocalEditRoot)
			continue
		}

		// TOCTOU re-check: re-stat immediately before deleting, since the
		// node-status round trip took real time.
		fresh, err := os.Lstat(c.rec.LocalPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				stats.SkippedAlreadyGone++
				continue
			}
			stats.Errors++
			_, _ = fmt.Fprintf(w, "[ERROR] re-stat %s: %v\n", c.rec.LocalPath, err)
			continue
		}
		if fresh.Size() != c.rec.SizeBytes || fresh.ModTime().Unix() != c.rec.MtimeUnix {
			stats.SkippedFileChanged++
			_, _ = fmt.Fprintf(w, "[SKIP] %s: changed since it was queued (size/mtime mismatch), refusing to delete\n", c.rec.LocalPath)
			continue
		}

		if dryRun {
			stats.Pruned++
			stats.FreedBytes += c.rec.SizeBytes
			_, _ = fmt.Fprintf(w, "[DRY-RUN] would prune %s (%d bytes)\n", c.rec.LocalPath, c.rec.SizeBytes)
			continue
		}
		if err := os.Remove(c.rec.LocalPath); err != nil {
			stats.Errors++
			_, _ = fmt.Fprintf(w, "[ERROR] remove %s: %v\n", c.rec.LocalPath, err)
			continue
		}
		stats.Pruned++
		stats.FreedBytes += c.rec.SizeBytes
		_, _ = fmt.Fprintf(w, "[PRUNED] %s (%d bytes)\n", c.rec.LocalPath, c.rec.SizeBytes)
	}

	return stats, nil
}

// withinRoot reports whether path resolves (after following symlinks) to
// somewhere under resolvedRoot (already itself resolved by the caller).
// Deliberately not a lexical filepath.Rel/HasPrefix check on the raw
// strings -- that would miss a symlink inside LocalEditRoot pointing
// outside it, which is exactly why storage.Guard.CheckWrite canonicalizes
// first on the server side (and internal/ingest/naming_test.go's own
// TestRenderPathTraversalSequenceNotStripped documents that a rendered path
// segment can carry an unstripped ".." through to here).
func withinRoot(resolvedRoot, path string) (bool, error) {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return false, nil // the root itself is never a prunable file
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false, nil
	}
	return true, nil
}

// PrintReport writes s to w in the same fixed, readable format `prune` has
// always used.
func PrintReport(w io.Writer, s Stats, dryRun bool) {
	if dryRun {
		_, _ = fmt.Fprintln(w, "branchdam-agent prune (DRY RUN -- nothing deleted)")
	} else {
		_, _ = fmt.Fprintln(w, "branchdam-agent prune")
	}
	_, _ = fmt.Fprintf(w, "  evaluated: %d, pruned: %d, freed: %d bytes\n", s.Evaluated, s.Pruned, s.FreedBytes)
	_, _ = fmt.Fprintf(w, "  skipped: sidecar=%d already-gone=%d too-young=%d not-verified=%d file-changed=%d outside-root=%d unexpected-symlink=%d\n",
		s.SkippedSidecar, s.SkippedAlreadyGone, s.SkippedTooYoung, s.SkippedNotVerified, s.SkippedFileChanged, s.SkippedOutsideRoot, s.SkippedUnexpectedSymlink)
	if s.Errors > 0 {
		_, _ = fmt.Fprintf(w, "  errors: %d -- see [ERROR] lines above\n", s.Errors)
	}
}
