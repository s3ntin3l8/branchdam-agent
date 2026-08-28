// Command branchdam-agent is the workstation agent for branchDAM (phase 10
// of the original spec). Subcommands landed so far: `preflight` (M0),
// `luminar-sync` (M4), `ingest` (M1's headless ingest-core driver, plus
// `-offline` for M2's offline queue), `queue-drain` (M2, drains queue.db on
// reconnect), `prune` (branchdam#230-adjacent -- deletes an offline-ingested
// file's LocalEditRoot mirror once the server confirms the Tier-3 copy is
// live and hash-verified; see internal/config.PruneConfig's doc comment for
// scope), `tray` (M1's tray shell, issue #3 -- a thin driver over
// the same internal/ingest.Engine `ingest` uses; see internal/tray's doc
// comment), and `update` (the headless equivalent of the tray's
// "Install and restart" menu item, for hosts that never run a tray; see
// internal/selfupdate's doc comment). See docs/roadmap.md in the
// branchdam repo and this repo's CLAUDE.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// version is stamped at build time via -ldflags "-X main.version=...";
// defaults to "dev" for local builds.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	switch args[0] {
	case "preflight":
		return runPreflightCmd(args[1:])
	case "luminar-sync":
		return runLuminarSyncCmd(args[1:])
	case "ingest":
		return runIngestCmd(args[1:])
	case "queue-drain":
		return runQueueDrainCmd(args[1:])
	case "prune":
		return runPruneCmd(args[1:])
	case "tray":
		return runTrayCmd(args[1:])
	case "update":
		return runUpdateCmd(args[1:])
	case "init":
		return runInitCmd(args[1:])
	case "dialog":
		// Hidden -- see dialog.go's doc comment. Deliberately absent from
		// usage() below: this exists only to be re-exec'd by this same
		// binary, never invoked directly by an operator.
		return runDialogCmd(args[1:], realDialogFuncs)
	case "-h", "--help", "help":
		usage()
		return 0
	case "-version", "--version", "version":
		fmt.Println(version)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "branchdam-agent: unknown subcommand %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: branchdam-agent <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "subcommands:")
	fmt.Fprintln(os.Stderr, "  preflight     check server connectivity, exiftool, and path mappings")
	fmt.Fprintln(os.Stderr, "  luminar-sync  read a Luminar Neo catalog and emit EVENT_EDGE_ATTACHED for filename-inferred edit->source pairs")
	fmt.Fprintln(os.Stderr, "  ingest        ingest a card's contents: dual-copy verified write + submit (-offline for issue #4's offline queue flow)")
	fmt.Fprintln(os.Stderr, "  queue-drain   drain queue.db on reconnect: submit queued events, copy archive bytes, rebase to Tier-3")
	fmt.Fprintln(os.Stderr, "  prune         delete offline-ingested files' LocalEditRoot mirror once the server confirms them verified (-dry-run to preview)")
	fmt.Fprintln(os.Stderr, "  tray          run the tray-resident shell (windows/darwin only) over the same ingest core")
	fmt.Fprintln(os.Stderr, "  update        check for (and optionally apply) a newer release (-check, -yes; requires selfUpdate.enabled)")
	fmt.Fprintln(os.Stderr, "  init          write a starter config.yaml to get started (-force to overwrite an existing one)")
	fmt.Fprintln(os.Stderr, "  version       print the agent's own version")
}

func runPreflightCmd(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	timeout := fs.Duration("timeout", 10*time.Second, "server request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	start := time.Now()

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Printf("branchdam-agent preflight\n=========================\n[FAIL] resolve config path: %v\n", err)
		return 1
	}

	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Printf("branchdam-agent preflight\n=========================\n[FAIL] load config %q: %v\n", resolvedPath, err)
		return 1
	}

	var client helloCaller
	if cfg.Server.APIKey != "" {
		client = branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	checks, ok := runPreflightChecks(ctx, cfg, client, defaultLookPath, defaultRunVersion)
	printPreflightReport(os.Stdout, resolvedPath, checks, ok, time.Since(start))

	if !ok {
		return 1
	}
	return 0
}
