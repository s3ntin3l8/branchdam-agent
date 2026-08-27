package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/prune"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// runPruneCmd implements `branchdam-agent prune` (branchdam#230-adjacent,
// see internal/config.PruneConfig's doc comment for exactly what this
// does and does not cover). One pass by default; -watch loops with
// -interval between passes, mirroring queue-drain's shape. The pass logic
// itself lives in internal/prune -- this file is flag parsing plus the
// config/queue/client wiring that logic needs.
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

	stats, err := prune.Pass(ctx, w, client, store, cfg, time.Now(), dryRun)
	if err != nil {
		_, _ = fmt.Fprintf(w, "[FAIL] prune pass: %v\n", err)
		return 1
	}
	prune.PrintReport(w, stats, dryRun)
	return 0
}
