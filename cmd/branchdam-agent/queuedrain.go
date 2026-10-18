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
	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
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
// -retry-failed first resets every archive copy and rebase the drainer
// marked permanently FAILED back to pending (queue.Store.ResetFailed), for
// use once an operator has fixed the cause recorded in
// archive_copy_last_error / rebase_last_error.
func runQueueDrainCmd(args []string) int {
	fs := flag.NewFlagSet("queue-drain", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	watch := fs.Bool("watch", false, "keep draining in a loop (Ctrl-C to stop) instead of a single pass")
	interval := fs.Duration("interval", 5*time.Second, "poll interval between passes when -watch is set")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-pass timeout")
	retryFailed := fs.Bool("retry-failed", false, "first put every permanently FAILED archive copy and rebase back to pending, then drain (use after fixing the cause recorded in archive_copy_last_error / rebase_last_error)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent queue-drain: resolve config path: %v\n", err)
		return 1
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent queue-drain: load config %q: %v\n", resolvedPath, err)
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

	if *retryFailed {
		if code := resetFailed(os.Stdout, store, *timeout); code != 0 {
			return code
		}
	}

	client := newServerClient(cfg, resolvedPath)

	// Queued uploads are not bound by -timeout (an upload can outlast it);
	// they stop when the process does. With pauseUploadOnMetered set they
	// are also held back while the link is metered -- this command has no
	// tray in front of it to skip the pass.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	passOpts := []ingest.DrainOption{ingest.WithUploadContext(ctx)}
	if cfg.Ingest.PauseUploadOnMetered {
		passOpts = append(passOpts, ingest.WithUploadGate(netgate.IsMetered))
	}

	if !*watch {
		return runDrainPass(os.Stdout, client, store, cfg.AgentID, *timeout, passOpts...)
	}

	_, _ = fmt.Fprintf(os.Stdout, "branchdam-agent queue-drain -watch: polling every %s (Ctrl-C to stop)\n", *interval)
	for {
		runDrainPass(os.Stdout, client, store, cfg.AgentID, *timeout, passOpts...)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(*interval):
		}
	}
}

// resetFailed implements -retry-failed: queue.Store's ResetFailed,
// reported on w.
func resetFailed(w io.Writer, store *queue.Store, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	n, err := store.ResetFailed(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "[FAIL] reset failed rows: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(w, "retry-failed: reset %d failed archive copies, %d failed rebases and %d failed uploads to pending\n", n.ArchiveCopies, n.Rebases, n.Uploads)
	return 0
}

func runDrainPass(w io.Writer, client *branchdam.Client, store *queue.Store, agentID string, timeout time.Duration, opts ...ingest.DrainOption) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stats, err := ingest.Drain(ctx, client, store, agentID, nil, opts...)
	if err != nil {
		_, _ = fmt.Fprintf(w, "[FAIL] drain pass: %v\n", err)
		return 1
	}

	if stats.HandshakeErr != nil {
		_, _ = fmt.Fprintf(w, "handshake: unreachable (%v) -- log-only, continuing (plan contract gap 2: not a resume mechanism)\n", stats.HandshakeErr)
	} else {
		_, _ = fmt.Fprintf(w, "handshake: ok, server version %s\n", stats.ServerVersion)
	}
	_, _ = fmt.Fprintf(w, "node_created submitted: %d, archive copies done: %d, archive copies permanently failed: %d, rebases done: %d, rebases permanently failed: %d, still pending: %d\n",
		stats.NodeCreatedSent, stats.ArchiveCopiesDone, stats.ArchiveCopiesFailed, stats.RebasesDone, stats.RebasesFailed, stats.Remaining)
	if stats.UploadsDone > 0 || stats.UploadsFailed > 0 {
		_, _ = fmt.Fprintf(w, "queued uploads done: %d, permanently failed: %d\n", stats.UploadsDone, stats.UploadsFailed)
	}
	if stats.UploadsFailed > 0 {
		_, _ = fmt.Fprintln(w, "one or more queued uploads hit a permanent failure (a local copy changed or gone since it was queued, a refused hash, or a fatal server answer) -- see queue.db's upload_last_error column; fix the cause, then run queue-drain -retry-failed")
	}
	if stats.RowsCollected > 0 {
		_, _ = fmt.Fprintf(w, "old finished rows collected: %d\n", stats.RowsCollected)
	}
	if stats.ArchiveCopiesFailed > 0 {
		_, _ = fmt.Fprintln(w, "one or more archive copies hit a permanent failure (a different file already at the archive path, a local copy edited since ingest, or an archive copy that read back wrong twice in a row) -- nothing was overwritten; see queue.db's archive_copy_last_error column; fix the cause, then run queue-drain -retry-failed")
	}
	if stats.RebasesFailed > 0 {
		_, _ = fmt.Fprintln(w, "one or more rows hit a permanent (non-retryable) rebase failure -- see queue.db's rebase_last_error column; fix the cause, then run queue-drain -retry-failed")
	}
	return 0
}
