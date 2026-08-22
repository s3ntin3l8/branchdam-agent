// Command branchdam-agent is the workstation agent for branchDAM (phase 10
// of the original spec). This M0 milestone ships only the `preflight`
// subcommand -- the first thing an operator runs, and the first thing every
// support question starts with. Tray UI, card ingest, the offline queue,
// and the DaVinci/Luminar integrations are later milestones; see
// docs/roadmap.md in the branchdam repo and this repo's CLAUDE.md.
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
	fmt.Fprintln(os.Stderr, "  preflight   check server connectivity, exiftool, and path mappings")
	fmt.Fprintln(os.Stderr, "  version     print the agent's own version")
}

func runPreflightCmd(args []string) int {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	timeout := fs.Duration("timeout", 10*time.Second, "server request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	start := time.Now()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("branchdam-agent preflight\n=========================\n[FAIL] load config %q: %v\n", *configPath, err)
		return 1
	}

	var client helloCaller
	if cfg.Server.APIKey != "" {
		client = branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	checks, ok := runPreflightChecks(ctx, cfg, client, defaultLookPath, defaultRunVersion)
	printPreflightReport(os.Stdout, *configPath, checks, ok, time.Since(start))

	if !ok {
		return 1
	}
	return 0
}
