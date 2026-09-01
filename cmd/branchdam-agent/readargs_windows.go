//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// runReadArgsCmd reads a JSON sidecar file containing args and re-execs
// the agent with those args.
//
// On Windows, there is no syscall.Exec equivalent: Go's standard
// library only exposes CreateProcess via os/exec, and any exec.Cmd
// spawns a child. Windows' Run-key semantics differ from Unix's
// launchd/session-manager model -- the Run-key launches a child of
// the user's logon session directly, and SIGTERM-equivalent (logoff)
// propagation to descendants is the kernel's job, not the parent's
// -- so the macOS hermes concern (launchd tracking the wrapper, not
// the tray) does not apply on Windows. A short-lived wrapper process
// is acceptable here; the tray sees the inherited stdio, env, and
// session, and on logoff the session tear-down propagates to all
// descendants regardless of who spawned them.
func runReadArgsCmd(args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: branchdam-agent -read-args <sidecar-path>\n")
		return 2
	}

	sidecarPath := args[0]

	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: read sidecar: %v\n", err)
		return 1
	}

	var sidecarArgs []string
	if err := json.Unmarshal(data, &sidecarArgs); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: parse sidecar: %v\n", err)
		return 1
	}

	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: resolve executable: %v\n", err)
		return 1
	}

	newArgs := append([]string{execPath}, sidecarArgs...)
	cmd := exec.Command(newArgs[0], newArgs[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: re-exec failed: %v\n", err)
		return 1
	}
	return 0
}
