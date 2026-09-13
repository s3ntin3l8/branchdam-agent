package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
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
// (lookPathFunc, runVersionFunc) -- so notifyStartupFailure's own logic
// (message formatting) is unit-testable without ever invoking a real
// dialog backend.
//
// The first parameter is the caller's context, threaded all the way
// through to the subprocess so cancellation actually tears down the
// re-exec'd `dialog` (Hermes review on #134: a context.Background in
// runDialogSubprocess meant the gate's 60s confirmTimeout never reached
// the dialog subprocess, which then hung up to the full 5-minute
// dialogTimeout bound). Callers that don't care about cancellation
// (notifyStartupFailure) pass context.Background.
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
// context for `trayConfirm`, context.Background for the startup-failure
// dialog). It's threaded through to exec.CommandContext so cancellation of
// ctx tears down the dialog subprocess -- without this, the gate's 60s
// timeout had no effect on the 5-minute dialog subprocess, which then hung
// until the OS cleaned up. The dialogTimeout here is a SECOND, longer
// deadline applied to ctx via WithTimeout, so the subprocess is bounded
// both by the caller's deadline AND by dialogTimeout, whichever fires first.
// This is the right shape for trayConfirm (where the caller's
// confirmTimeout is the binding bound) and harmless for
// notifyStartupFailure (where context.Background never cancels, so
// dialogTimeout alone bounds the wait).
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
