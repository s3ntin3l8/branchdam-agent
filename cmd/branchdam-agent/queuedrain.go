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
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// runQueueDrainCmd implements `branchdam-agent queue-drain`: the reconnect
// side of issue #4's offline flow. One pass by default (submits any pending
// EVENT_NODE_CREATED, copies any pending archive bytes, and rebases any row
// whose archive copy is verified and whose node_created dwell has elapsed --
// see internal/ingest/drain.go's Drain and MinRebaseDwell); -watch loops
// with -interval between passes until interrupted. Per-row exponential
// backoff (queue.Store's next-attempt columns, internal/ingest/drain.go's
// backoffFor) is what keeps repeated passes from hammering a server or NAS
// mount that's still down -- Drain itself is stateless across calls.
func runQueueDrainCmd(args []string) int {
	fs := flag.NewFlagSet("queue-drain", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	watch := fs.Bool("watch", false, "keep draining in a loop (Ctrl-C to stop) instead of a single pass")
	interval := fs.Duration("interval", 5*time.Second, "poll interval between passes when -watch is set")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-pass timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent queue-drain: load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Offline.QueueDBPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent queue-drain: offline.queueDbPath must be set in config")
		return 1
	}
	if cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent queue-drain: server.apiKey is empty in config")
		return 1
	}

	store, err := queue.Open(cfg.Offline.QueueDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent queue-drain: open queue db %q: %v\n", cfg.Offline.QueueDBPath, err)
		return 1
	}
	defer func() { _ = store.Close() }()

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)

	if !*watch {
		return runDrainPass(os.Stdout, client, store, cfg.AgentID, *timeout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, _ = fmt.Fprintf(os.Stdout, "branchdam-agent queue-drain -watch: polling every %s (Ctrl-C to stop)\n", *interval)
	for {
		runDrainPass(os.Stdout, client, store, cfg.AgentID, *timeout)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*interval):
		}
	}
}

func runDrainPass(w io.Writer, client *branchdam.Client, store *queue.Store, agentID string, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stats, err := ingest.Drain(ctx, client, store, agentID, nil)
	if err != nil {
		_, _ = fmt.Fprintf(w, "[FAIL] drain pass: %v\n", err)
		return 1
	}

	if stats.HandshakeErr != nil {
		_, _ = fmt.Fprintf(w, "handshake: unreachable (%v) -- log-only, continuing (plan contract gap 2: not a resume mechanism)\n", stats.HandshakeErr)
	} else {
		_, _ = fmt.Fprintf(w, "handshake: ok, server version %s\n", stats.ServerVersion)
	}
	_, _ = fmt.Fprintf(w, "node_created submitted: %d, archive copies done: %d, rebases done: %d, rebases permanently failed: %d, still pending: %d\n",
		stats.NodeCreatedSent, stats.ArchiveCopiesDone, stats.RebasesDone, stats.RebasesFailed, stats.Remaining)
	if stats.RebasesFailed > 0 {
		_, _ = fmt.Fprintln(w, "one or more rows hit a permanent (non-retryable) rebase failure -- see queue.db's rebase_last_error column; these require operator intervention, not another drain pass")
	}
	return 0
}
