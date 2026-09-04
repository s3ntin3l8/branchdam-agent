package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/eject"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// runIngestCmd implements `branchdam-agent ingest --card <path> --config
// <path>`: the headless entry point issue #2 requires so the ingest core is
// testable in CI without a UI, and the same code path a later tray PR
// drives. One-shot: ingest --card takes an already-resolved card directory
// (a mounted volume, or a fixture directory in a test) and processes it
// once, then exits -- polling for card insertion/removal
// (internal/ingest.Detector) is separate, real, tested library code this
// subcommand does not yet drive automatically (that wiring belongs to the
// tray, per issue #2's stated scope).
func runIngestCmd(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	cardPath := fs.String("card", "", "path to the card's root directory (a mounted volume, or a fixture directory)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall ingest run timeout")
	offline := fs.Bool("offline", false, "offline mode (issue #4): write the local copy only, queue the archive copy and EVENT_NODE_CREATED in queue.db for a later `queue-drain` -- see offline.* in config and docs/offline-queue.md")
	upload := fs.Bool("upload", false, "direct HTTP streaming upload to POST /api/v1/agent/upload instead of local archiveRoot dual-write")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cardPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: -card is required")
		return 2
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest: resolve config path: %v\n", err)
		return 1
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest: load config %q: %v\n", resolvedPath, err)
		return 1
	}
	if *upload {
		cfg.Ingest.UploadStream = true
	}
	if cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: server.apiKey is empty in config")
		return 1
	}
	if cfg.Ingest.LocalEditRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: ingest.localEditRoot must be set in config")
		return 1
	}
	if !cfg.Ingest.UploadStream && cfg.Ingest.ArchiveRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: ingest.archiveRoot must be set in config when not using -upload")
		return 1
	}

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	engine := ingest.NewEngine(client, cfg.AgentID, cfg.Ingest, cfg.PathMappings)
	// This process exits right after this command returns -- a pooled
	// exiftool subprocess left running would just be orphaned, not reaped
	// by a finalizer that never gets a chance to run (see
	// internal/exiftool's package doc for why sync.Pool eviction alone
	// can't be relied on for that).
	defer engine.Exiftool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Synchronize naming template from server handshake if available
	if hs, err := client.Handshake(ctx, branchdam.HandshakeRequest{AgentID: cfg.AgentID}); err == nil && hs.NamingTemplate != "" {
		cfg.Ingest.PathTemplate = hs.NamingTemplate
		engine.Ingest.PathTemplate = hs.NamingTemplate
	}

	if *offline {
		return runIngestOffline(ctx, engine, cfg, *cardPath)
	}

	start := time.Now()
	result, err := engine.IngestCard(ctx, *cardPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest: %v\n", err)
		return 1
	}

	ok := printIngestReport(os.Stdout, *cardPath, result, time.Since(start))
	if !ok {
		return 1
	}
	if cfg.Ingest.AutoEject {
		if err := eject.Eject(*cardPath); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent ingest: auto-eject failed: %v\n", err)
		}
	}
	return 0
}

// runIngestOffline is `ingest -offline`'s body: open queue.db, wire it and
// Tier0ContainerRoot into the Engine, and run IngestCardOffline instead of
// IngestCard. Unlike the online path, a per-file Err here is the only
// failure mode that matters at ingest time -- everything else (submission,
// archive copy, rebase) is queued for `queue-drain` to finish later.
func runIngestOffline(ctx context.Context, engine *ingest.Engine, cfg config.Config, cardPath string) int {
	if cfg.Offline.QueueDBPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest -offline: offline.queueDbPath must be set in config")
		return 1
	}
	if cfg.Offline.Tier0ContainerRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest -offline: offline.tier0ContainerRoot must be set in config")
		return 1
	}

	store, err := queue.Open(cfg.Offline.QueueDBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest -offline: open queue db %q: %v\n", cfg.Offline.QueueDBPath, err)
		return 1
	}
	defer func() { _ = store.Close() }()

	engine.Queue = store
	engine.Tier0ContainerRoot = cfg.Offline.Tier0ContainerRoot

	start := time.Now()
	result, err := engine.IngestCardOffline(ctx, cardPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest -offline: %v\n", err)
		return 1
	}

	exitCode := printOfflineIngestReport(os.Stdout, cardPath, result, time.Since(start))
	if exitCode == 0 && cfg.Ingest.AutoEject {
		if err := eject.Eject(cardPath); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent ingest -offline: auto-eject failed: %v\n", err)
		}
	}
	return exitCode
}

// printOfflineIngestReport mirrors printIngestReport's shape, adapted to
// OfflineFileResult's fields: there is no EventID to print (see that type's
// doc comment), and "safe to eject" only ever gates on the local copy
// verifying -- the archive copy and rebase are `queue-drain`'s job, not
// this command's.
func printOfflineIngestReport(w io.Writer, cardPath string, result ingest.OfflineCardResult, elapsed time.Duration) int {
	_, _ = fmt.Fprintln(w, "branchdam-agent ingest -offline")
	_, _ = fmt.Fprintln(w, "===============================")
	_, _ = fmt.Fprintf(w, "card: %s\n\n", cardPath)

	ok := true
	queued, skipped, failed := 0, 0, 0
	for _, f := range result.Files {
		switch {
		case f.Err != nil:
			failed++
			ok = false
			_, _ = fmt.Fprintf(w, "[FAIL] %s: %v\n", f.SourcePath, f.Err)
		case f.Skipped:
			skipped++
			_, _ = fmt.Fprintf(w, "[SKIP] %s: %s\n", f.SourcePath, f.SkipReason)
		default:
			queued++
			resumed := ""
			if f.AlreadyQueued {
				resumed = " (resumed from a previous run)"
			}
			inline := "not yet submitted"
			if f.SubmittedInline {
				inline = "submitted"
			}
			_, _ = fmt.Fprintf(w, "[QUEUED] %s -> %s (nodeUuid=%s, EVENT_NODE_CREATED %s, localVerify=%s%s)\n",
				f.SourcePath, f.LocalPath, f.NodeUUID, inline, f.LocalVerify.Method, resumed)
		}
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "%d queued, %d skipped, %d failed (%s)\n", queued, skipped, failed, elapsed.Round(time.Millisecond))
	if ok {
		_, _ = fmt.Fprintln(w, "safe to eject -- archive copy and server rebase are pending; run `branchdam-agent queue-drain` on reconnect")
	} else {
		_, _ = fmt.Fprintln(w, "NOT safe to eject -- one or more files failed")
	}
	if !ok {
		return 1
	}
	return 0
}

// printIngestReport writes a per-file summary and returns whether every
// file that should have been submitted succeeded (a sidecar's deliberate
// skip does not count as a failure).
func printIngestReport(w io.Writer, cardPath string, result ingest.CardResult, elapsed time.Duration) bool {
	_, _ = fmt.Fprintln(w, "branchdam-agent ingest")
	_, _ = fmt.Fprintln(w, "======================")
	_, _ = fmt.Fprintf(w, "card: %s\n\n", cardPath)

	ok := true
	submitted, skipped, failed := 0, 0, 0
	for _, f := range result.Files {
		switch {
		case f.Err != nil:
			failed++
			ok = false
			_, _ = fmt.Fprintf(w, "[FAIL] %s: %v\n", f.SourcePath, f.Err)
		case f.Skipped:
			skipped++
			_, _ = fmt.Fprintf(w, "[SKIP] %s: %s\n", f.SourcePath, f.SkipReason)
		default:
			submitted++
			gps := ""
			if f.GPSSource != "" {
				gps = fmt.Sprintf(", gps=%s", f.GPSSource)
			}
			_, _ = fmt.Fprintf(w, "[OK]   %s -> %s (eventId=%s, archiveVerify=%s, localVerify=%s%s)\n",
				f.SourcePath, f.ArchivePath, f.EventID, f.ArchiveVerify.Method, f.LocalVerify.Method, gps)
		}
	}

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "%d submitted, %d skipped, %d failed (%s)\n", submitted, skipped, failed, elapsed.Round(time.Millisecond))
	if ok {
		_, _ = fmt.Fprintln(w, "safe to eject")
	} else {
		_, _ = fmt.Fprintln(w, "NOT safe to eject -- one or more files failed")
	}
	return ok
}
