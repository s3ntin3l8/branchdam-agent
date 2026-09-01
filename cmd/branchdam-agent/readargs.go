package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// readArgsFromSidecar reads a JSON-encoded args array from sidecarPath
// and returns the parsed args. Errors carry an exitCode (2 for usage,
// 1 for I/O / parse failures) so the caller can thread them back
// through to os.Exit. Extracted from the platform-specific runReadArgsCmd
// variants so the security-critical arg-parsing path (arity guard, file
// read, JSON parse) is testable on any host without actually exec'ing
// a child or replacing the process image.
//
// The arity guard rejects both 0 args (caller did not pass a sidecar
// path) and >1 args (caller passed more than one path, which the
// arg-injection fix's plist/reg value can never legitimately produce).
// Either case is a usage error: returning exitCode 2 mirrors the GNU
// convention `2 = command-line usage error`.
func readArgsFromSidecar(args []string) (parsed []string, exitCode int) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: branchdam-agent -read-args <sidecar-path>\n")
		return nil, 2
	}

	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: read sidecar: %v\n", err)
		return nil, 1
	}

	var sidecarArgs []string
	if err := json.Unmarshal(data, &sidecarArgs); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent -read-args: parse sidecar: %v\n", err)
		return nil, 1
	}
	return sidecarArgs, 0
}
