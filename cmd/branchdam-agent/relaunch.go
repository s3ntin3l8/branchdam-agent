package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/s3ntin3l8/branchdam-agent/internal/selfupdate"
)

// relaunchSelf starts a fresh copy of the just-updated binary at selfExe
// with args, and returns once the new process has been started -- the
// caller exits immediately after. selfExe MUST have been captured with
// os.Executable() before the binary was swapped by an in-place update,
// and the caller MUST have already released the status server's listener
// (via StatusServer.Listen/Serve, not ListenAndServe) -- otherwise the
// child's own bind fails immediately, since branchdam-agent tray is
// single-instance by construction (see runTrayCmd).
//
// Spawn-then-exit on every platform, never syscall.Exec: syscall.Exec
// doesn't exist on Windows at all, so using it on darwin/linux would
// still leave Windows needing this path regardless -- and on Windows the
// running .exe has already been renamed aside to a hidden .old by the
// time Apply succeeds, so re-executing over the same process image isn't
// an option there anyway.
func relaunchSelf(selfExe string, args []string) error {
	if runtime.GOOS == "darwin" {
		if bundle := selfupdate.BundlePath(selfExe); bundle != "" {
			return relaunchMacBundle(bundle, args)
		}
	}

	cmd := exec.Command(selfExe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch %s: %w", selfExe, err)
	}
	return nil
}

// relaunchMacBundle restarts a bundled tray via "open -n -a <bundle>
// --args ...", not a bare exec.Command(selfExe, ...), so LaunchServices
// attaches a proper application session to the new process instead of it
// running as a bare child of this one.
//
// The -n ("always open a new instance") flag is required. relaunchSelf
// is called while the old process is still alive (runTrayCmd's call
// stack hasn't returned yet). Without -n, LaunchServices sees the
// bundle as running, activates the old instance, and ignores --args —
// the freshly-updated binary never starts and the tray disappears.
// With -n, LaunchServices creates a new instance regardless. The port
// conflict that -n would normally cause is already mitigated: runTrayCmd
// calls stop() + wg.Wait() (cmd/branchdam-agent/tray.go:283-284)
// before relaunchSelf, fully releasing the status-server listener so
// the new instance binds successfully. Single-instance is preserved
// because only one tray is actively running at any time — the old
// process exits immediately after relaunchSelf returns. See issue #107.
func relaunchMacBundle(bundlePath string, args []string) error {
	cmd := exec.Command("open", relaunchMacBundleOpenArgs(bundlePath, args)...)
	if err := startRelaunchCmd(cmd); err != nil {
		return fmt.Errorf("relaunch via open -n -a %s: %w", bundlePath, err)
	}
	return nil
}

// relaunchMacBundleOpenArgs builds the argv passed to `open` for the
// macOS bundled relaunch path. Extracted as a pure function so the
// argv (and specifically the presence of the -n flag) is testable on
// every CI host, not just darwin.
func relaunchMacBundleOpenArgs(bundlePath string, args []string) []string {
	return append([]string{"-n", "-a", bundlePath, "--args"}, args...)
}

// startRelaunchCmd runs cmd.Start() and returns the error. Indirected
// through a package var so tests can substitute a no-op (running
// \`open\` in a unit test on Linux is a non-starter, but the wrapper
// and the argv are what we want to pin).
var startRelaunchCmd = func(cmd *exec.Cmd) error {
	return cmd.Start()
}
