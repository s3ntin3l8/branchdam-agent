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
func relaunchMacBundle(bundlePath string, args []string) error {
	cmdArgs := append([]string{"-n", "-a", bundlePath, "--args"}, args...)
	cmd := exec.Command("open", cmdArgs...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("relaunch via open -a %s: %w", bundlePath, err)
	}
	return nil
}
