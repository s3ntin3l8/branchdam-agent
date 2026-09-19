package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// runPairCmd implements `branchdam-agent pair <branchdam-url>`: paste a
// branchdam:// URL from branchDAM's Companion Pairing modal into the
// workstation agent and have it persist server.baseUrl, server.apiKey,
// and agentId into the config in one shot. Validates the credentials by
// calling POST /api/v1/agent/hello before writing -- if the server
// rejects the key (wrong pairing pasted, server rotated mid-paste,
// pairing revoked), the operator sees the error and the config is not
// touched.
//
// Replaces the manual "edit config.yaml with server.baseUrl, server.apiKey,
// agentId" workflow that issue #453's PR B called out as a UX gap -- the
// workstation agent had no way to learn its paired agent_id automatically,
// so the server's /agent/handshake cross-check (which requires
// body.AgentID == Principal.Name) would 403 every state-mutating request
// for a manually-pasted per-device key. Pairing via this subcommand
// sets agentId from the same source the QR payload carries, so the
// cross-check passes byte-identically to a phone-app QR scan.
func runPairCmd(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	timeout := fs.Duration("timeout", 10*time.Second, "server request timeout for the validation Hello() call")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rawURL := fs.Arg(0)
	if rawURL == "" {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: usage: pair <branchdam-url>\n\n")
		fmt.Fprintf(os.Stderr, "Paste the branchdam:// URL from branchDAM's Companion Pairing modal\n")
		fmt.Fprintf(os.Stderr, "(Settings -> Companion Pairing -> Pair new device -> copy URL).\n")
		return 2
	}

	parsed, err := branchdam.ParsePairingURL(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: %v\n", err)
		return 1
	}

	// Validate before touching the config. A pasted key that the server
	// rejects (rotated, revoked, mistyped) must not silently overwrite a
	// working credential set in the file.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := branchdam.New(parsed.Server, parsed.Key)
	if _, err := client.Hello(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: server at %s rejected the key: %v\n", parsed.Server, err)
		return 1
	}

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: resolve config path: %v\n", err)
		return 1
	}

	// Patch all three fields atomically. config.Patch writes back at mode
	// 0600 (it embeds a plaintext API key), so a pre-existing config with
	// world-readable perms won't be widened by this call -- but we do
	// refuse to proceed if the file is already world-readable, since
	// writing a secret into a leaky file is worse than the current
	// state.
	if err := refuseIfPermsLeaky(path); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: %v\n", err)
		return 1
	}

	changes := map[string]any{
		"server.baseUrl": parsed.Server,
		"server.apiKey":  parsed.Key,
		"agentId":        parsed.Agent,
	}
	if err := config.Patch(path, changes); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent pair: write config: %v\n", err)
		return 1
	}

	fmt.Printf("branchdam-agent pair: paired with %s as agentId %q\n", parsed.Server, parsed.Agent)
	fmt.Printf("  credentials written to %s (mode 0600).\n", path)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Run: branchdam-agent preflight -config " + path)
	fmt.Println("     to confirm server reachability, agentId round-trip, and path mappings.")
	fmt.Println("  2. Restart any running tray/ingest so they pick up the new credentials.")
	return 0
}

// refuseIfPermsLeaky returns an error if the file at path exists and is
// group- or world-readable. config.Patch writes back at mode 0600 so a
// newly-created file is always tight, but it doesn't widen the perms of
// a pre-existing leaky file -- and writing a plaintext API key into one
// is strictly worse than leaving whatever was there before. Mirrors the
// posture in internal/config.Load's checkConfigFilePerms (called by
// every config-touching subcommand) but refuses rather than warns, since
// the user's intent on `pair` is specifically to embed a secret.
//
// Windows is exempt: per the same rationale internal/config's
// checkFilePermissions documents (issue #126), Windows exposes ACLs
// rather than POSIX group/world mode bits, so any Stat-derived mask
// would always trip 0o077 on a writable file and reject every legitimate
// `pair` invocation. The "embed a secret" risk doesn't apply on Windows
// the same way -- ACLs are the right surface, and this CLI doesn't own
// ACL management. Skip silently rather than fail-closed.
func refuseIfPermsLeaky(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // Patch will create it at 0600.
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%s is group/world readable (mode %#o); refusing to embed a plaintext API key. Run: chmod 600 %s", path, mode, path)
	}
	return nil
}
