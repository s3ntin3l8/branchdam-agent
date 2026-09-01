//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// runReadArgsCmd reads a JSON sidecar file containing args and re-execs
// the agent with those args, replacing the current process image via
// syscall.Exec.
//
// On Unix, syscall.Exec replaces the current process image entirely,
// so launchd (macOS) or the user's session manager (Linux) sees the
// tray process directly, not a wrapper. This is the critical property
// the autostart arg-injection fix relies on: SIGTERM on logout is
// delivered to the tray, and os.Executable()/pid match what the tray
// itself reports. exec.Command would have spawned a child that
// inherited the launchd/session-manager slot from the wrapper, which
// launchd would SIGTERM at logout -- the tray would never see it.
//
// The path that exec.Command would have returned (a child exit code)
// is gone: syscall.Exec either succeeds and never returns, or fails
// and returns an error. The caller treats a syscall.Exec error as
// a startup failure.
func runReadArgsCmd(args []string) int {
	sidecarArgs, code := readArgsFromSidecar(args)
	if code != 0 {
		return code
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: resolve executable: %v\n", err)
		return 1
	}

	// Build the new argv: executable path + decoded args. argv[0] is
	// conventionally the program name, so we pass execPath as argv[0].
	newArgs := append([]string{execPath}, sidecarArgs...)

	// Inherit stdio via os.Stdin/Stdout/Stderr FDs (0, 1, 2). Passing
	// them as nil env to syscall.Exec inherits the parent environment.
	if err := syscall.Exec(execPath, newArgs, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: exec: %v\n", err)
		return 1
	}
	// Unreachable: syscall.Exec does not return on success.
	return 0
}
