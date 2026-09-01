package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// runReadArgsCmd reads a JSON sidecar file containing args and re-execs
// the agent with those args. This is used by the autostart mechanism
// to avoid embedding potentially dangerous characters in shell commands.
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

	// Find the executable path
	execPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: resolve executable: %v\n", err)
		return 1
	}

	// Build the new args: executable path + decoded args
	newArgs := append([]string{execPath}, sidecarArgs...)

	// Re-exec with the decoded args
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
