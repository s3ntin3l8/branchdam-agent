package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// preflightCheck is one line of preflight's report: a human-readable status
// ("OK"/"WARN"/"FAIL") plus a message. Kept as data rather than printed
// directly by runPreflightChecks so the checks themselves are testable
// without capturing stdout.
type preflightCheck struct {
	Status  string // "OK", "WARN", or "FAIL"
	Message string
}

func (c preflightCheck) String() string {
	return fmt.Sprintf("[%-4s] %s", c.Status, c.Message)
}

// helloCaller is the subset of *branchdam.Client's surface runPreflightChecks
// needs, so tests can substitute a fake without a real HTTP server.
type helloCaller interface {
	Hello(ctx context.Context) (*branchdam.HelloResponse, error)
}

// lookPathFunc/runVersionFunc are exec.LookPath/exec.Command("...", "-ver")
// indirections so tests can simulate "exiftool missing" or "exiftool
// present" deterministically, independent of the actual test machine's PATH.
type lookPathFunc func(file string) (string, error)
type runVersionFunc func(path string) (string, error)

func defaultLookPath(file string) (string, error) { return exec.LookPath(file) }

func defaultRunVersion(path string) (string, error) {
	out, err := exec.Command(path, "-ver").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// runPreflightChecks runs every preflight check against cfg and returns the
// full report plus whether every check passed (WARN does not fail the
// overall report; only FAIL does -- an operator with no exiftool installed
// yet, or no path mappings configured yet, can still confirm server
// connectivity works). client may be nil when cfg.Server.APIKey is empty --
// in that case the connectivity check fails locally without ever dialing
// out.
func runPreflightChecks(
	ctx context.Context,
	cfg config.Config,
	client helloCaller,
	lookPath lookPathFunc,
	runVersion runVersionFunc,
) ([]preflightCheck, bool) {
	var checks []preflightCheck
	ok := true

	// 0. General config sanity -- see config.Validate's own doc comment for
	// why this runs before anything else: an unexpanded ${VAR} placeholder
	// passes a naive `!= ""` check and then fails downstream in a way that
	// looks like a server misconfiguration (a 503 from a too-short
	// apiKey), not the local one it actually is. Anything under "server."
	// fails the report outright since it would otherwise surface as a
	// confusing connectivity failure in check 1 below; everything else is
	// advisory. A Problem marked Advisory() stays advisory even on a
	// server.* field -- used today for cleartext http on a loopback host,
	// which is legitimate local-dev posture (issue #96) and must not block
	// an agent run.
	for _, p := range cfg.Validate() {
		status := "WARN"
		if !p.Advisory() && strings.HasPrefix(p.Field, "server.") {
			status = "FAIL"
			ok = false
		}
		checks = append(checks, preflightCheck{status, p.String()})
	}

	// 1. Server reachability + auth + version. This is preflight's
	// headline check -- the plan's M0 gate is specifically "preflight
	// against the live server returns the server version."
	switch {
	case cfg.Server.APIKey == "":
		checks = append(checks, preflightCheck{"FAIL", "server.apiKey is empty in config"})
		ok = false
	case client == nil:
		checks = append(checks, preflightCheck{"FAIL", "no branchDAM client configured"})
		ok = false
	default:
		resp, err := client.Hello(ctx)
		switch {
		case err != nil:
			checks = append(checks, preflightCheck{"FAIL", fmt.Sprintf("POST %s/api/v1/agent/hello: %v", cfg.Server.BaseURL, err)})
			ok = false
		case resp.Version == "":
			// Guards against the hello-vs-handshake field-name mixup this
			// client's conformance tests pin: an empty Version here with a
			// 2xx response is a strong signal something is reading the
			// wrong JSON field, not that the server has no version.
			checks = append(checks, preflightCheck{"FAIL", fmt.Sprintf("server at %s returned ok=%v but an empty version", cfg.Server.BaseURL, resp.OK)})
			ok = false
		default:
			checks = append(checks, preflightCheck{"OK", fmt.Sprintf("server reachable at %s, version %s", cfg.Server.BaseURL, resp.Version)})
		}
	}

	// 2. exiftool on PATH (or at cfg.Ingest.ExiftoolPath, when an operator
	// has configured a non-default binary -- internal/ingest.NewExiftoolAt
	// resolves the same way, so this check reports on the exact binary
	// ingest will actually pool). Non-fatal: the agent falls back to
	// fast_hash-only indexing (matching branchDAM's own probe.go doc
	// comment) when it's absent, same as a machine running branchDAM's own
	// CI Go job.
	exiftoolBin := "exiftool"
	if cfg.Ingest.ExiftoolPath != "" {
		exiftoolBin = cfg.Ingest.ExiftoolPath
	}
	if path, err := lookPath(exiftoolBin); err != nil {
		checks = append(checks, preflightCheck{"WARN", fmt.Sprintf("exiftool not found (looked for %q) -- pHash RAW fallback and metadata extraction will be skipped", exiftoolBin)})
	} else if ver, err := runVersion(path); err != nil {
		checks = append(checks, preflightCheck{"WARN", fmt.Sprintf("exiftool found at %s but `-ver` failed: %v", path, err)})
	} else {
		checks = append(checks, preflightCheck{"OK", fmt.Sprintf("exiftool %s found at %s", ver, path)})
	}

	// 3. Workstation -> container path mappings. Required before any real
	// ingest -- agent event payloads must carry server-container paths, and
	// there is no server-side rewrite pass on them (see internal/config's
	// doc comment).
	if len(cfg.PathMappings) == 0 {
		checks = append(checks, preflightCheck{"WARN", "no pathMappings configured -- agent event payloads require server-container paths"})
	} else {
		for _, m := range cfg.PathMappings {
			checks = append(checks, preflightCheck{"OK", fmt.Sprintf("path mapping: %s  ->  %s", m.WorkstationPath, m.ContainerPath)})
		}
	}

	return checks, ok
}

// printPreflightReport writes checks to w in a fixed, readable format --
// this is the first thing an operator runs and the first thing every
// support question starts with, so it's plain status lines, not a table or
// JSON blob a person has to parse.
func printPreflightReport(w io.Writer, configPath string, checks []preflightCheck, ok bool, elapsed time.Duration) {
	_, _ = fmt.Fprintln(w, "branchdam-agent preflight")
	_, _ = fmt.Fprintln(w, "=========================")
	_, _ = fmt.Fprintf(w, "config: %s\n\n", configPath)
	for _, c := range checks {
		_, _ = fmt.Fprintln(w, c.String())
	}
	_, _ = fmt.Fprintln(w)
	if ok {
		_, _ = fmt.Fprintf(w, "preflight passed (%s)\n", elapsed.Round(time.Millisecond))
	} else {
		_, _ = fmt.Fprintf(w, "preflight FAILED (%s)\n", elapsed.Round(time.Millisecond))
	}
}
