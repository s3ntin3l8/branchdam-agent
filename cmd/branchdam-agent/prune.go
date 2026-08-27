package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// defaultPruneMinAgeHours is applied when Prune.Enabled is true and
// Prune.MinAgeHours is left at its zero value -- config.Load applies no
// defaulting of its own (see PollIntervalSecs' identical pattern), so this
// lives at the point of use.
const defaultPruneMinAgeHours = 24

// pruneNodeStatusBatchSize chunks NodeUUID lookups to stay under the
// server's own per-request cap (branchDAM's agentNodeStatusMaxUUIDs, 200).
const pruneNodeStatusBatchSize = 200

// runPruneCmd implements `branchdam-agent prune` (branchdam#230-adjacent,
// see internal/config.PruneConfig's doc comment for exactly what this
// does and does not cover). One pass by default; -watch loops with
// -interval between passes, mirroring queue-drain's shape.
func runPruneCmd(args []string) int {
	flagSet := flag.NewFlagSet("prune", flag.ContinueOnError)
	configPath := flagSet.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	dryRun := flagSet.Bool("dry-run", false, "report what would be pruned without deleting anything")
	watch := flagSet.Bool("watch", false, "keep pruning in a loop (Ctrl-C to stop) instead of a single pass")
	interval := flagSet.Duration("interval", 30*time.Minute, "poll interval between passes when -watch is set")
	timeout := flagSet.Duration("timeout", 2*time.Minute, "per-pass timeout")
	if err := flagSet.Parse(args); err != nil {
		return 2
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent prune: resolve config path: %v\n", err)
		return 1
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent prune: load config %q: %v\n", resolvedPath, err)
		return 1
	}
	if !cfg.Prune.Enabled {
		fmt.Fprintln(os.Stderr, "branchdam-agent prune: prune.enabled is false in config -- refusing to run")
		return 1
	}
	if cfg.Offline.QueueDBPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent prune: offline.queueDbPath must be set in config -- only offline-ingested files (tracked in queue.db) are ever prune-eligible")
		return 1
	}
	if cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent prune: server.apiKey is empty in config")
		return 1
	}
	if cfg.Ingest.LocalEditRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent prune: ingest.localEditRoot must be set in config")
		return 1
	}

	store, err := queue.Open(cfg.Offline.QueueDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent prune: open queue db %q: %v\n", cfg.Offline.QueueDBPath, err)
		return 1
	}
	defer func() { _ = store.Close() }()

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)

	if !*watch {
		return runPrunePassCmd(os.Stdout, client, store, cfg, *dryRun, *timeout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, _ = fmt.Fprintf(os.Stdout, "branchdam-agent prune -watch: polling every %s (Ctrl-C to stop)\n", *interval)
	for {
		runPrunePassCmd(os.Stdout, client, store, cfg, *dryRun, *timeout)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*interval):
		}
	}
}

func runPrunePassCmd(w io.Writer, client *branchdam.Client, store *queue.Store, cfg config.Config, dryRun bool, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stats, err := runPrunePass(ctx, w, client, store, cfg, time.Now(), dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(w, "[FAIL] prune pass: %v\n", err)
		return 1
	}
	printPruneReport(w, stats, dryRun)
	return 0
}

// pruneStats is one pass's accounting -- printed in full so a skip is always
// visible and attributable, never silently dropped.
type pruneStats struct {
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

// runPrunePass implements one prune pass end to end. Deliberately narrow in
// scope: only queue.db rows (offline-ingested files) are ever considered --
// a plain online `ingest` has no durable local-path ledger to work from at
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
func runPrunePass(ctx context.Context, w io.Writer, client *branchdam.Client, store *queue.Store, cfg config.Config, now time.Time, dryRun bool) (pruneStats, error) {
	var stats pruneStats

	minAgeHours := cfg.Prune.MinAgeHours
	if minAgeHours <= 0 {
		minAgeHours = defaultPruneMinAgeHours
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
	for start := 0; start < len(candidates); start += pruneNodeStatusBatchSize {
		end := start + pruneNodeStatusBatchSize
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

func printPruneReport(w io.Writer, s pruneStats, dryRun bool) {
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
