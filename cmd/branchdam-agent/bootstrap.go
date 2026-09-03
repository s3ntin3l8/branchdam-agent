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
//
// The first parameter is the caller's context, threaded all the way
// through to the subprocess so cancellation actually tears down the
// re-exec'd `dialog` (Hermes review on #134: a context.Background in
// runDialogSubprocess meant the gate's 60s confirmTimeout never reached
// the dialog subprocess, which then hung up to the full 5-minute
// dialogTimeout bound). Callers that don't care about cancellation
// (bootstrap, notifyStartupFailure) pass context.Background.
type dialogRunner func(ctx context.Context, args ...string) (stdout string, exitCode int, err error)

// selfDialogRunner returns a dialogRunner that re-execs selfExe. selfExe
// empty (os.Executable itself failed before this could even be attempted)
// yields a runner that always fails fast with a descriptive error, so
// every caller's existing "dialog failed" handling covers this case too
// without a separate nil/empty check at each call site.
func selfDialogRunner(selfExe string) dialogRunner {
	if selfExe == "" {
		return func(_ context.Context, _ ...string) (string, int, error) {
			return "", -1, errors.New("own executable path is unknown")
		}
	}
	return func(ctx context.Context, args ...string) (string, int, error) {
		return runDialogSubprocess(ctx, selfExe, args...)
	}
}

// runDialogSubprocess re-execs selfExe as `dialog <args...>`, returning its
// trimmed stdout and exit code. err is non-nil only for a failure to even
// start/run the subprocess (selfExe not executable, context deadline) -- a
// dialog that rendered and was answered, canceled, or failed to display
// all come back as (value, dialogExit*, nil); see dialog.go's
// dialogExitOK/Failed/Canceled.
//
// ctx is the caller's context (typically the gate's confirmTimeout-bounded
// context for `trayConfirm`, context.Background for the bootstrap
// wizard / startup-failure dialog). It's threaded through to
// exec.CommandContext so cancellation of ctx tears down the dialog
// subprocess -- without this, the gate's 60s timeout had no effect on
// the 5-minute dialog subprocess, which then hung until the OS cleaned
// up. The dialogTimeout here is a SECOND, longer deadline applied to
// ctx via WithTimeout, so the subprocess is bounded both by the
// caller's deadline AND by dialogTimeout, whichever fires first.
// This is the right shape for trayConfirm (where the caller's
// confirmTimeout is the binding bound) and harmless for
// bootstrapConfigInteractive (where context.Background never
// cancels, so dialogTimeout alone bounds the wait).
func runDialogSubprocess(ctx context.Context, selfExe string, args ...string) (stdout string, exitCode int, err error) {
	subCtx, cancel := context.WithTimeout(ctx, dialogTimeout)
	defer cancel()

	cmd := exec.CommandContext(subCtx, selfExe, append([]string{"dialog"}, args...)...)
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
	if _, _, err := run(context.Background(), "-kind", "error", "-title", "branchDAM Agent", "-message", full); err != nil {
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

// pathMappingContainerKey is bootstrapPrompts' one synthetic key: it is
// never passed to config.Patch directly (pathMappings is a list, not a
// scalar leaf), only used to look the answer back up in
// bootstrapConfigInteractive once every prompt has been walked -- see
// that function for how it becomes the wizard's one PathMapping entry.
const pathMappingContainerKey = "pathMapping.containerPath"

// bootstrapPrompts is the first-run wizard's fixed question order: server
// URL, API key, the two ingest roots, and the server-container path the
// archive root maps to. That last prompt exists because a completed
// wizard with no pathMappings entry would otherwise launch a tray whose
// first real ingest fails with ErrNoPathMapping (internal/ingest/pathmap.go)
// -- a confusing downstream error that would undercut this whole feature's
// "works out of the box" premise. Package-level so tests can assert its
// shape without invoking any dialog.
var bootstrapPrompts = []bootstrapPrompt{
	{"server.baseUrl", "entry", "branchDAM Agent Setup (1/5)", "branchDAM server URL:", "http://localhost:8080"},
	{"server.apiKey", "password", "branchDAM Agent Setup (2/5)", "Agent API key (from your branchDAM server, 32+ characters):", ""},
	{"ingest.archiveRoot", "directory", "branchDAM Agent Setup (3/5)", "Select the archive (NAS) folder:", ""},
	{"ingest.localEditRoot", "directory", "branchDAM Agent Setup (4/5)", "Select the local edit (scratch) folder:", ""},
	{pathMappingContainerKey, "entry", "branchDAM Agent Setup (5/5)", "Server-container path for the archive folder just selected (e.g. /storage/archive):", "/storage/archive"},
}

// bootstrapConfigInteractive writes a starter config to path (see
// writeStarterConfig, shared with `init`), then walks bootstrapPrompts via
// run, applying every answer with a single config.Patch call at the end --
// including a pathMappings entry built from the archive root and container
// path answers together, since PathMapping is a struct, not a scalar leaf
// any single prompt's dotted key can address directly. Returns
// errBootstrapCanceled if the operator dismissed any prompt; any other
// error means a dialog itself failed to render.
func bootstrapConfigInteractive(run dialogRunner, path string) error {
	if err := writeStarterConfig(path); err != nil {
		return err
	}

	answers := make(map[string]string, len(bootstrapPrompts))
	for _, p := range bootstrapPrompts {
		args := []string{"-kind", p.kind, "-title", p.title}
		if p.msg != "" {
			args = append(args, "-message", p.msg)
		}
		if p.def != "" {
			args = append(args, "-default", p.def)
		}
		value, exitCode, err := run(context.Background(), args...)
		if err != nil {
			return fmt.Errorf("run setup dialog for %s: %w", p.key, err)
		}
		switch exitCode {
		case dialogExitOK:
			answers[p.key] = value
		case dialogExitCanceled:
			return errBootstrapCanceled
		default:
			return fmt.Errorf("setup dialog for %s failed (exit %d)", p.key, exitCode)
		}
	}

	changes := map[string]any{
		"server.baseUrl":       answers["server.baseUrl"],
		"server.apiKey":        answers["server.apiKey"],
		"ingest.archiveRoot":   answers["ingest.archiveRoot"],
		"ingest.localEditRoot": answers["ingest.localEditRoot"],
		"pathMappings": []config.PathMapping{{
			WorkstationPath: answers["ingest.archiveRoot"],
			ContainerPath:   answers[pathMappingContainerKey],
		}},
	}
	if err := config.Patch(path, changes); err != nil {
		return fmt.Errorf("save setup answers: %w", err)
	}
	return nil
}
