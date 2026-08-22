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
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
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
	configPath := fs.String("config", "config.yaml", "path to config file")
	cardPath := fs.String("card", "", "path to the card's root directory (a mounted volume, or a fixture directory)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall ingest run timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cardPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: -card is required")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent ingest: load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: server.apiKey is empty in config")
		return 1
	}
	if cfg.Ingest.ArchiveRoot == "" || cfg.Ingest.LocalEditRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent ingest: ingest.archiveRoot and ingest.localEditRoot must both be set in config")
		return 1
	}

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	engine := ingest.NewEngine(client, cfg.AgentID, cfg.Ingest, cfg.PathMappings)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

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
