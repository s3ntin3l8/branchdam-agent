package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// dialogTimeout bounds how long a re-exec'd `dialog` subprocess is allowed
// to block waiting for the operator -- generous enough for a human to
// read and respond, but not infinite, so a headless environment (no
// display, no zenity backend available) can't wedge the parent process
// forever if some backend hangs instead of failing fast.
const dialogTimeout = 5 * time.Minute

// dialogRunner shows one dialog (by re-exec'ing `dialog <args...>`, see
// dialog.go's doc comment for why) and returns its trimmed stdout and exit
// code. Indirected -- like this repo's other OS-boundary function types
// (lookPathFunc, runVersionFunc) -- so bootstrapConfigInteractive and
// notifyStartupFailure's own logic (prompt order, cancel/error handling,
// message formatting) is unit-testable without ever invoking a real
// dialog backend.
type dialogRunner func(args ...string) (stdout string, exitCode int, err error)

// selfDialogRunner returns a dialogRunner that re-execs selfExe. selfExe
// empty (os.Executable itself failed before this could even be attempted)
// yields a runner that always fails fast with a descriptive error, so
// every caller's existing "dialog failed" handling covers this case too
// without a separate nil/empty check at each call site.
func selfDialogRunner(selfExe string) dialogRunner {
	if selfExe == "" {
		return func(args ...string) (string, int, error) {
			return "", -1, errors.New("own executable path is unknown")
		}
	}
	return func(args ...string) (string, int, error) {
		return runDialogSubprocess(selfExe, args...)
	}
}

// runDialogSubprocess re-execs selfExe as `dialog <args...>`, returning its
// trimmed stdout and exit code. err is non-nil only for a failure to even
// start/run the subprocess (selfExe not executable, context deadline) -- a
// dialog that rendered and was answered, canceled, or failed to display
// all come back as (value, dialogExit*, nil); see dialog.go's
// dialogExitOK/Failed/Canceled.
func runDialogSubprocess(selfExe string, args ...string) (stdout string, exitCode int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, selfExe, append([]string{"dialog"}, args...)...)
	out, runErr := cmd.Output()
	if runErr == nil {
		return strings.TrimRight(string(out), "\n"), dialogExitOK, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return strings.TrimRight(string(out), "\n"), exitErr.ExitCode(), nil
	}
	return "", -1, runErr
}

// notifyStartupFailure shows a best-effort error dialog naming logPath (if
// resolved) alongside message. Never surfaces its own error to the caller
// -- a dialog that fails to render (no display, zenity not installed, an
// unexpected exec failure) must never turn a config error into a
// different one; the caller's own error, log line, and exit code already
// captured that.
func notifyStartupFailure(run dialogRunner, message, logPath string) {
	full := message
	if logPath != "" {
		full = fmt.Sprintf("%s\n\nSee %s for details.", message, logPath)
	}
	if _, _, err := run("-kind", "error", "-title", "branchDAM Agent", "-message", full); err != nil {
		slog.Warn("could not show startup-error dialog", "err", err)
	}
}

// errBootstrapCanceled is returned by bootstrapConfigInteractive when the
// operator dismissed any prompt in the setup wizard. The starter config
// bootstrapConfigInteractive already wrote is left in place either way --
// a canceled wizard still leaves something to hand-edit, matching `init`'s
// headless equivalent.
var errBootstrapCanceled = errors.New("first-run setup canceled")

// bootstrapPrompt is one step of bootstrapConfigInteractive's wizard.
type bootstrapPrompt struct {
	key   string // dotted config key, applied via config.Patch
	kind  string // dialog.go's -kind: "entry", "password", or "directory"
	title string
	msg   string
	def   string // pre-filled default, entry/password only
}

// bootstrapPrompts is the first-run wizard's fixed question order: server
// URL, API key, then the two ingest roots preflight and the tray both
// require. Package-level so tests can assert its shape without invoking
// any dialog.
var bootstrapPrompts = []bootstrapPrompt{
	{"server.baseUrl", "entry", "branchDAM Agent Setup (1/4)", "branchDAM server URL:", "http://localhost:8080"},
	{"server.apiKey", "password", "branchDAM Agent Setup (2/4)", "Agent API key (from your branchDAM server, 32+ characters):", ""},
	{"ingest.archiveRoot", "directory", "branchDAM Agent Setup (3/4) -- select the archive (NAS) folder", "", ""},
	{"ingest.localEditRoot", "directory", "branchDAM Agent Setup (4/4) -- select the local edit (scratch) folder", "", ""},
}

// bootstrapConfigInteractive writes a starter config to path (see
// writeStarterConfig, shared with `init`), then walks bootstrapPrompts via
// run, applying every answer with a single config.Patch call at the end.
// Returns errBootstrapCanceled if the operator dismissed any prompt; any
// other error means a dialog itself failed to render.
func bootstrapConfigInteractive(run dialogRunner, path string) error {
	if err := writeStarterConfig(path); err != nil {
		return err
	}

	changes := make(map[string]any, len(bootstrapPrompts))
	for _, p := range bootstrapPrompts {
		args := []string{"-kind", p.kind, "-title", p.title}
		if p.msg != "" {
			args = append(args, "-message", p.msg)
		}
		if p.def != "" {
			args = append(args, "-default", p.def)
		}
		value, exitCode, err := run(args...)
		if err != nil {
			return fmt.Errorf("run setup dialog for %s: %w", p.key, err)
		}
		switch exitCode {
		case dialogExitOK:
			changes[p.key] = value
		case dialogExitCanceled:
			return errBootstrapCanceled
		default:
			return fmt.Errorf("setup dialog for %s failed (exit %d)", p.key, exitCode)
		}
	}

	if err := config.Patch(path, changes); err != nil {
		return fmt.Errorf("save setup answers: %w", err)
	}
	return nil
}
