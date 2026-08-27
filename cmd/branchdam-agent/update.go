package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/selfupdate"
)

// updateExitUpdateAvailable is returned by `update -check` when a newer
// release exists but nothing was applied -- distinct from 0 (up to date)
// and 1 (failure) so a script can branch on it without parsing stdout.
const updateExitUpdateAvailable = 10

// runUpdateCmd implements `branchdam-agent update`: the headless
// equivalent of the tray's "Install and restart" menu item, for hosts
// that run preflight/ingest/queue-drain and never start a tray (Linux, or
// a Windows console-only install). Gated on selfUpdate.enabled exactly
// like the tray's background check -- an explicit invocation of this
// subcommand is itself the operator's consent, but the config flag is
// still what decides whether this binary is willing to contact GitHub at
// all, so there is exactly one switch controlling that, not two that
// could disagree.
func runUpdateCmd(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	checkOnly := fs.Bool("check", false, "check for an update and report it, without applying")
	yes := fs.Bool("yes", false, "apply without prompting for confirmation")
	timeout := fs.Duration("timeout", 5*time.Minute, "timeout for the check/apply network calls")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: load config %q: %v\n", *configPath, err)
		return 1
	}
	if !cfg.SelfUpdate.Enabled {
		fmt.Fprintln(os.Stderr, "branchdam-agent update: selfUpdate.enabled is false in config; refusing to contact GitHub")
		return 1
	}

	up, err := selfupdate.NewUpdater(cfg.SelfUpdate.RepoOrDefault())
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := up.Check(ctx, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: %v\n", err)
		return 1
	}
	fmt.Println(result.String())

	if !result.UpdateFound {
		return 0
	}
	if *checkOnly {
		return updateExitUpdateAvailable
	}
	if !*yes && !confirm(os.Stdin, os.Stdout, fmt.Sprintf("Install %s -> %s? [y/N] ", result.CurrentVersion, result.LatestVersion)) {
		fmt.Println("branchdam-agent update: not applying")
		return 0
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: resolve own executable: %v\n", err)
		return 1
	}
	layout, err := selfupdate.DetectLayout(execPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: %v\n", err)
		return 1
	}

	appliedVersion, err := up.Apply(ctx, version, layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: apply: %v\n", err)
		return 1
	}

	fmt.Printf("branchdam-agent update: applied %s\n", appliedVersion)
	return 0
}

func confirm(in *os.File, out *os.File, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
