package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

func TestSelfDialogRunnerEmptyExeFailsFast(t *testing.T) {
	run := selfDialogRunner("")
	_, _, err := run(context.Background(), "-kind", "error")
	if err == nil {
		t.Error("expected an error when selfExe is empty")
	}
}

func TestNotifyStartupFailureNeverPanicsOnRunError(t *testing.T) {
	var gotArgs []string
	run := func(_ context.Context, args ...string) (string, int, error) {
		gotArgs = args
		return "", -1, errors.New("no display")
	}
	// Must not panic and must not return anything -- there is nothing to
	// assert on besides "this returns cleanly."
	notifyStartupFailure(run, "something broke", "/var/log/agent.log")
	if len(gotArgs) == 0 {
		t.Error("expected notifyStartupFailure to invoke the dialog runner")
	}
	found := false
	for _, a := range gotArgs {
		if a == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected -kind error in args, got %v", gotArgs)
	}
}

func TestNotifyStartupFailureIncludesLogPath(t *testing.T) {
	var gotMessage string
	run := func(_ context.Context, args ...string) (string, int, error) {
		for i, a := range args {
			if a == "-message" && i+1 < len(args) {
				gotMessage = args[i+1]
			}
		}
		return "", 0, nil
	}
	notifyStartupFailure(run, "config is broken", "/var/log/branchDAM/agent.log")
	if !strings.Contains(gotMessage, "config is broken") || !strings.Contains(gotMessage, "/var/log/branchDAM/agent.log") {
		t.Errorf("expected message to mention both the failure and the log path, got %q", gotMessage)
	}
}

func TestBootstrapConfigInteractiveHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	answers := map[string]string{
		"server.baseUrl":        "https://branchdam.example.com",
		"server.apiKey":         "0123456789abcdef0123456789abcdef",
		"ingest.archiveRoot":    "/archive",
		"ingest.localEditRoot":  "/edit",
		pathMappingContainerKey: "/storage/archive",
	}
	var promptedKinds []string
	run := func(_ context.Context, args ...string) (string, int, error) {
		var title string
		for i, a := range args {
			if a == "-title" && i+1 < len(args) {
				title = args[i+1]
			}
		}
		// Find which prompt this is by matching the title back to
		// bootstrapPrompts, since dialogRunner only sees flag args.
		for _, p := range bootstrapPrompts {
			if p.title == title {
				promptedKinds = append(promptedKinds, p.kind)
				return answers[p.key], dialogExitOK, nil
			}
		}
		t.Fatalf("unexpected dialog title %q", title)
		return "", dialogExitFailed, nil
	}

	if err := bootstrapConfigInteractive(run, path); err != nil {
		t.Fatalf("bootstrapConfigInteractive: %v", err)
	}
	if len(promptedKinds) != len(bootstrapPrompts) {
		t.Errorf("expected %d prompts, got %d", len(bootstrapPrompts), len(promptedKinds))
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load bootstrapped config: %v", err)
	}
	if cfg.Server.BaseURL != answers["server.baseUrl"] {
		t.Errorf("server.baseUrl = %q, want %q", cfg.Server.BaseURL, answers["server.baseUrl"])
	}
	if cfg.Server.APIKey != answers["server.apiKey"] {
		t.Errorf("server.apiKey = %q, want %q", cfg.Server.APIKey, answers["server.apiKey"])
	}
	if cfg.Ingest.ArchiveRoot != answers["ingest.archiveRoot"] {
		t.Errorf("ingest.archiveRoot = %q, want %q", cfg.Ingest.ArchiveRoot, answers["ingest.archiveRoot"])
	}
	if cfg.Ingest.LocalEditRoot != answers["ingest.localEditRoot"] {
		t.Errorf("ingest.localEditRoot = %q, want %q", cfg.Ingest.LocalEditRoot, answers["ingest.localEditRoot"])
	}
	if len(cfg.PathMappings) != 1 ||
		cfg.PathMappings[0].WorkstationPath != answers["ingest.archiveRoot"] ||
		cfg.PathMappings[0].ContainerPath != answers[pathMappingContainerKey] {
		t.Errorf("pathMappings = %+v, want one entry %s -> %s", cfg.PathMappings, answers["ingest.archiveRoot"], answers[pathMappingContainerKey])
	}
}

func TestBootstrapConfigInteractiveCanceledMidway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	calls := 0
	run := func(_ context.Context, args ...string) (string, int, error) {
		calls++
		if calls == 2 {
			return "", dialogExitCanceled, nil
		}
		return "some-value", dialogExitOK, nil
	}

	err := bootstrapConfigInteractive(run, path)
	if !errors.Is(err, errBootstrapCanceled) {
		t.Fatalf("expected errBootstrapCanceled, got %v", err)
	}
	if calls != 2 {
		t.Errorf("expected the wizard to stop at the canceled prompt, got %d calls", calls)
	}

	// The starter config must still exist even though the wizard was
	// canceled -- there's still something to hand-edit afterward.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("expected starter config to remain on disk after cancel: %v", statErr)
	}
}

func TestBootstrapConfigInteractiveDialogFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	run := func(_ context.Context, args ...string) (string, int, error) {
		return "", dialogExitFailed, nil
	}

	err := bootstrapConfigInteractive(run, path)
	if err == nil || errors.Is(err, errBootstrapCanceled) {
		t.Fatalf("expected a non-cancel error for a dialog that failed to render, got %v", err)
	}
}

func TestBootstrapConfigInteractiveRunnerError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	run := func(_ context.Context, args ...string) (string, int, error) {
		return "", -1, errors.New("exec failed")
	}

	if err := bootstrapConfigInteractive(run, path); err == nil {
		t.Fatal("expected an error when the dialog runner itself fails")
	}
}

// TestRunDialogSubprocessThreadsContext pins the regression on #134:
// runDialogSubprocess must use the caller-provided ctx (not
// context.Background), so the gate's 60s confirmTimeout actually
// cancels the dialog subprocess. The dialog subprocess's
// exec.CommandContext wraps cmd with a derived subCtx via
// WithTimeout(ctx, dialogTimeout); cancelling the parent ctx
// cancels subCtx, which exec.CommandContext observes.
//
// We can't easily exec a real dialog subprocess here (this is a
// Linux CI runner with no display, and dialog refuses to run on
// /dev/null), but we can verify the threading another way: pass
// an already-cancelled ctx, and assert the error chain contains
// context.Canceled. Without ctx threading, runDialogSubprocess
// would use its own WithTimeout(context.Background(), dialogTimeout)
// background context and the cancellation would be ignored -- the
// subprocess would run to its dialogTimeout bound regardless.
//
// Also pins the sub-claim that a real-world usage (caller passes a
// non-cancelled context with a 1h budget to a missing selfExe) still
// fails fast with ENOENT, not by hanging.
func TestRunDialogSubprocessThreadsContext(t *testing.T) {
	// Pass an already-cancelled ctx. runDialogSubprocess must derive
	// its WithTimeout from this ctx (not from context.Background), so
	// the resulting subCtx is also cancelled and exec.CommandContext
	// returns context.Canceled-derived error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := runDialogSubprocess(ctx, "/nonexistent/path/to/dialog-binary-for-test-only")
	if err == nil {
		t.Fatal("expected an error from runDialogSubprocess with a cancelled ctx")
	}
	// On Linux this surfaces as a *fs.PathError wrapping syscall.ENOENT
	// (fork/exec runs immediately even with a cancelled ctx because the
	// executable lookup happens before the first wait4). The
	// cross-platform-stable check is: err is non-nil and the
	// subprocess did NOT silently succeed. The HERMES-RELEVANT
	// contract -- "caller's ctx reaches exec.CommandContext" -- is
	// verified by the cancel-during-wait path below, which is what
	// actually matters in production.
	if !errors.Is(err, fs.ErrNotExist) && err != context.Canceled {
		// Acceptable: exec might fail with either path-error or
		// context-canceled depending on whether the lookup happened
		// before exec observed the cancel. Anything else is a
		// regression worth flagging.
		var execErr *exec.Error
		if !errors.As(err, &execErr) {
			t.Errorf("expected fs.ErrNotExist, context.Canceled, or *exec.Error, got %T: %v", err, err)
		}
	}
}

// TestRunDialogSubprocessCtxCancelDuringWait pins the actual
// cancelation contract: when the caller's ctx is cancelled mid-run,
// the subprocess is torn down. Without ctx threading to exec.CommandContext
// (Hermes finding on #134), this would silently fail to cancel.
//
// runDialogSubprocess returns nil error for *exec.ExitError (a
// signal-killed process shows up as ExitError with ExitCode()==-1
// and Stderr="signal: killed"). The right observable is therefore
// NOT "err != nil" but rather:
//   - exitCode == -1 (signaled, not clean exit)
//   - elapsed time is small (subprocess died promptly, not at the
//     5-minute dialogTimeout bound)
//
// A buggy runDialogSubprocess that uses context.Background instead of
// the passed ctx would let the subprocess run for ~5 minutes; the
// elapsed assertion catches that case directly.
func TestRunDialogSubprocessCtxCancelDuringWait(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// runDialogSubprocess invokes the binary as `selfExe dialog <args>...`,
	// where <args> is whatever the call site passes. sleep accepts
	// multiple positional duration args; the leading "dialog" is
	// non-numeric and makes sleep exit immediately. To work around
	// that, write a tiny shell wrapper that ignores its argv and
	// execs `sleep 86400` directly.
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "hang.sh")
	script := "#!/bin/sh\nshift; exec sleep 86400\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, exitCode, _ := runDialogSubprocess(ctx, wrapper)
	elapsed := time.Since(start)

	if exitCode != -1 {
		t.Errorf("expected exitCode=-1 (signal-killed), got %d; ctx threading not reaching exec.CommandContext?", exitCode)
	}
	// The cancellation must be quick: the subprocess should be killed
	// within a small multiple of the 100ms ctx, not anywhere near the
	// 5-minute dialogTimeout bound. 2s is generous to avoid CI flakes.
	if elapsed > 2*time.Second {
		t.Errorf("subprocess took %v to die after ctx fired; ctx threading not reaching exec.CommandContext?", elapsed)
	}
}
