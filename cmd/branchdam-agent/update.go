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
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	checkOnly := fs.Bool("check", false, "check for an update and report it, without applying")
	yes := fs.Bool("yes", false, "apply (or roll back) without prompting for confirmation")
	rollback := fs.Bool("rollback", false, "restore the previously applied version, without checking for or applying any new update")
	timeout := fs.Duration("timeout", 5*time.Minute, "timeout for the check/apply network calls")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *rollback {
		return runUpdateRollbackCmd(*yes)
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: resolve config path: %v\n", err)
		return 1
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: load config %q: %v\n", resolvedPath, err)
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

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), *timeout)
	defer cancelCheck()

	result, err := up.Check(checkCtx, version)
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

	// Apply gets its own timeout, independent of the check's, so a slow
	// Check can't eat into the budget Apply needs to get through a
	// multi-step Windows sibling-then-primary swap -- see Apply's own doc
	// comment on why a cancel landing mid-swap is worse than a slow one.
	applyCtx, cancelApply := context.WithTimeout(context.Background(), *timeout)
	defer cancelApply()

	appliedVersion, err := up.Apply(applyCtx, version, layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update: apply: %v\n", err)
		return 1
	}

	fmt.Printf("branchdam-agent update: applied %s\n", appliedVersion)
	return 0
}

// runUpdateRollbackCmd implements `branchdam-agent update -rollback`: the
// headless equivalent of the tray's "Roll back to vX" menu item.
// Deliberately independent of selfUpdate.enabled and config entirely --
// unlike Check/Apply, Rollback makes no network call (it restores from a
// local ".previous" backup a prior Apply left on disk), so there is no
// config to load and nothing here needs to be gated by that flag. See
// selfupdateagent.go's RollbackAvailable/Rollback doc comments for the
// same reasoning on the tray side.
func runUpdateRollbackCmd(yes bool) int {
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update -rollback: resolve own executable: %v\n", err)
		return 1
	}
	layout, err := selfupdate.DetectLayout(execPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update -rollback: %v\n", err)
		return 1
	}

	version, err := selfupdate.PreviousVersion(layout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "branchdam-agent update -rollback: no previous version to roll back to")
		return 1
	}

	if !yes && !confirm(os.Stdin, os.Stdout, fmt.Sprintf("Roll back to %s? [y/N] ", version)) {
		fmt.Println("branchdam-agent update -rollback: not rolling back")
		return 0
	}

	applied, err := selfupdate.Rollback(layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent update -rollback: %v\n", err)
		return 1
	}

	fmt.Printf("branchdam-agent update: rolled back to %s\n", applied)
	return 0
}

func confirm(in *os.File, out *os.File, prompt string) bool {
	_, _ = fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
