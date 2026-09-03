package main

import (
	"context"
	"errors"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// fakeDialogRunner is a dialogRunner that returns whatever the test
// wants for any args. It records the call args so a test can assert
// which -kind / -title / -message the tray's confirm callback actually
// forwarded, not just that it returned the right bool.
type fakeDialogRunner struct {
	calls    [][]string
	stdout   string
	exitCode int
	err      error
}

func (f *fakeDialogRunner) Run(_ context.Context, args ...string) (string, int, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.stdout, f.exitCode, f.err
}

// TestTrayConfirmOKIsProceed: trayConfirm(run)("...", "...") returns
// true iff the dialog subprocess exits with dialogExitOK. This is the
// success path of every destructive click handler (issue #108 / E3
// #S2-14): a clean OK click must let the action proceed.
func TestTrayConfirmOKIsProceed(t *testing.T) {
	run := &fakeDialogRunner{exitCode: dialogExitOK}
	confirm := trayConfirm(run.Run)

	if !confirm(context.Background(), "Confirm prune", "Delete files?") {
		t.Fatal("expected trayConfirm to return true on dialogExitOK")
	}
}

// TestTrayConfirmCancelIsRefuse: dialogExitCanceled (the standard
// "operator clicked Cancel / closed the window" sentinel) must map to
// false so the gated click handler skips the destructive action.
func TestTrayConfirmCancelIsRefuse(t *testing.T) {
	run := &fakeDialogRunner{exitCode: dialogExitCanceled}
	confirm := trayConfirm(run.Run)

	if confirm(context.Background(), "Confirm prune", "Delete files?") {
		t.Fatal("expected trayConfirm to return false on dialogExitCanceled")
	}
}

// TestTrayConfirmFailedRenderIsRefuse pins the safety net: a non-zero,
// non-canceled exit (or any err from the subprocess) must be a refusal.
// The whole point of the gate is that a misconfigured host can never
// silently turn into a destructive-action launch, so "the dialog
// didn't even render" is the same shape as Cancel.
func TestTrayConfirmFailedRenderIsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *fakeDialogRunner
	}{
		{"unexpected nonzero exit", &fakeDialogRunner{exitCode: 99}},
		{"subprocess error", &fakeDialogRunner{err: errors.New("no display")}},
		{"both error and nonzero", &fakeDialogRunner{exitCode: 3, err: errors.New("oops")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confirm := trayConfirm(tc.run.Run)
			if confirm(context.Background(), "t", "b") {
				t.Fatal("expected trayConfirm to return false when the dialog did not render cleanly")
			}
		})
	}
}

// TestTrayConfirmForwardsTitleAndBody is the wiring-pin: the gated
// click handlers (run_supported.go's drain/prune/install/rollback
// cases) build the prompt with a specific title and body that the
// issue text mandates verbatim. If trayConfirm dropped or swapped
// those on the way to the subprocess, the operator would see the
// wrong text in the dialog.
func TestTrayConfirmForwardsTitleAndBody(t *testing.T) {
	run := &fakeDialogRunner{exitCode: dialogExitOK}
	confirm := trayConfirm(run.Run)
	confirm(context.Background(), "Confirm prune", "Delete files?")

	if len(run.calls) != 1 {
		t.Fatalf("expected exactly one dialog call, got %d", len(run.calls))
	}
	args := run.calls[0]
	seen := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		seen[args[i]] = args[i+1]
	}
	if got := seen["-kind"]; got != "question" {
		t.Errorf("got -kind %q, want %q", got, "question")
	}
	if got := seen["-title"]; got != "Confirm prune" {
		t.Errorf("got -title %q, want %q", got, "Confirm prune")
	}
	if got := seen["-message"]; got != "Delete files?" {
		t.Errorf("got -message %q, want %q", got, "Delete files?")
	}
}

// TestTrayIngestGateImportIsProceed covers the OK-button branch of the
// card-detection import confirmation dialog (issue #79): dialogExitOK
// (Import) means proceed with the volume's ingest.
func TestTrayIngestGateImportIsProceed(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitOK}
	s := newConfigSettings(path, cfg, runner, dialog.Run)
	gate := newTrayIngestGate(dialog.Run, s)

	proceed, err := gate.Confirm(context.Background(), "/media/card/CANON_R5", "CANON_R5")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed=true on dialogExitOK (Import)")
	}
}

// TestTrayIngestGateSkipThisTimeIsRefuse covers the Cancel-button branch:
// dialogExitCanceled (Skip this time) must mean refuse.
func TestTrayIngestGateSkipThisTimeIsRefuse(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitCanceled}
	s := newConfigSettings(path, cfg, runner, dialog.Run)
	gate := newTrayIngestGate(dialog.Run, s)

	proceed, err := gate.Confirm(context.Background(), "/media/card/CANON_R5", "CANON_R5")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if proceed {
		t.Fatal("expected proceed=false on dialogExitCanceled (Skip this time)")
	}
}

// TestTrayIngestGateAlwaysAutoImportPersistsAndProceeds covers the
// Extra-button branch: dialogExitExtraButton (Always auto-import) means
// proceed AND persist the volume into ingest.autoImportPaths so the next
// detection skips the dialog entirely.
func TestTrayIngestGateAlwaysAutoImportPersistsAndProceeds(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitExtraButton}
	s := newConfigSettings(path, cfg, runner, dialog.Run)
	gate := newTrayIngestGate(dialog.Run, s)

	proceed, err := gate.Confirm(context.Background(), "/media/card/CANON_R5", "CANON_R5")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed=true on dialogExitExtraButton (Always auto-import)")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Ingest.AutoImportPaths) != 1 || reloaded.Ingest.AutoImportPaths[0] != "/media/card/CANON_R5" {
		t.Errorf("expected autoImportPaths persisted to config, got %v", reloaded.Ingest.AutoImportPaths)
	}
}

// TestTrayIngestGateAutoImportPathsBypassesDialog covers the auto-import
// hit: a volume path already in ingest.autoImportPaths must short-circuit
// the dialog entirely and return proceed=true.
func TestTrayIngestGateAutoImportPathsBypassesDialog(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitCanceled}
	s := newConfigSettings(path, cfg, runner, dialog.Run)

	if err := s.SetStringSlice("ingest.autoImportPaths", []string{"/media/card/CANON_R5"}); err != nil {
		t.Fatal(err)
	}

	gate := newTrayIngestGate(dialog.Run, s)

	proceed, err := gate.Confirm(context.Background(), "/media/card/CANON_R5", "CANON_R5")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed=true when path is in autoImportPaths")
	}
	if len(dialog.calls) != 0 {
		t.Errorf("expected no dialog shown for auto-imported path, got %d calls", len(dialog.calls))
	}
}

// TestTrayIngestGateForwardsLabelsAndVolumeInfo verifies that the
// dialog's -title / -message / button labels reflect the operator-facing
// volume label and size when the lookup helper provides them -- a
// regression on this would make every card-detection prompt show the
// raw mount path instead of the human-readable label.
func TestTrayIngestGateForwardsLabelsAndVolumeInfo(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitOK}
	s := newConfigSettings(path, cfg, runner, dialog.Run)
	gate := newTrayIngestGate(dialog.Run, s)
	gate.lookup = func(_ context.Context, _ string) (string, string) {
		return "CANON R5", "32 GB"
	}

	proceed, err := gate.Confirm(context.Background(), "/Volumes/CANON_R5", "CANON_R5")
	if err != nil || !proceed {
		t.Fatalf("unexpected proceed=%v err=%v", proceed, err)
	}

	if len(dialog.calls) != 1 {
		t.Fatalf("expected 1 dialog call, got %d", len(dialog.calls))
	}
	args := dialog.calls[0]
	seen := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		seen[args[i]] = args[i+1]
	}
	if got := seen["-kind"]; got != "question" {
		t.Errorf("got -kind %q, want %q", got, "question")
	}
	if got := seen["-title"]; got != "New volume detected" {
		t.Errorf("got -title %q, want %q", got, "New volume detected")
	}
	if got := seen["-message"]; got != "New volume detected: CANON R5 (32 GB)" {
		t.Errorf("got -message %q, want %q", got, "New volume detected: CANON R5 (32 GB)")
	}
	if got := seen["-ok-label"]; got != "Import" {
		t.Errorf("got -ok-label %q, want %q", got, "Import")
	}
	if got := seen["-cancel-label"]; got != "Skip this time" {
		t.Errorf("got -cancel-label %q, want %q", got, "Skip this time")
	}
	if got := seen["-extra-button"]; got != "Always auto-import" {
		t.Errorf("got -extra-button %q, want %q", got, "Always auto-import")
	}
}

// TestTrayIngestGateFailedRenderIsRefuse pins the safety-net half of
// the gate: a dialog that didn't render must return an error and
// proceed=false, AND must NOT mark the volume as skipped (a transient
// dialog failure is recoverable -- a real Skip button is intentional).
// Manual TriggerIngest bypasses the gate entirely; that's intentional
// for the "Import from folder…" button, but the volume in this test
// exercises it to prove the bypass is wired right.
func TestTrayIngestGateFailedRenderIsRefuse(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{err: errors.New("no display")}
	s := newConfigSettings(path, cfg, runner, dialog.Run)
	gate := newTrayIngestGate(dialog.Run, s)

	proceed, err := gate.Confirm(context.Background(), "/media/card/CANON_R5", "CANON_R5")
	if err == nil {
		t.Fatal("expected error on failed dialog render")
	}
	if proceed {
		t.Fatal("expected proceed=false on failed dialog render")
	}

	runner.SetIngestGate(gate)
	summary := runner.TriggerDetectedIngest(context.Background(), "/media/card/CANON_R5")
	if summary.Err == nil {
		t.Fatal("expected TriggerDetectedIngest to return error when gate fails")
	}
	if runner.IsSkipped("/media/card/CANON_R5") {
		t.Fatal("expected volume NOT to be marked skipped after transient dialog failure")
	}

	manualSummary := runner.TriggerIngest(context.Background(), "/media/card/CANON_R5")
	if !manualSummary.OK() {
		t.Fatalf("expected manual TriggerIngest to proceed unconditionally, got %+v", manualSummary)
	}
}

// TestTrayPickDirectoryOK covers the success half of trayPickDirectory
// (issue #80): dialogExitOK from a `dialog -kind directory` returns the
// picked path on stdout.
func TestTrayPickDirectoryOK(t *testing.T) {
	run := &fakeDialogRunner{stdout: "/media/external/card\n", exitCode: dialogExitOK}
	pick := trayPickDirectory(run.Run)
	path, err := pick(context.Background(), "Import from folder…")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/media/external/card" {
		t.Errorf("got path %q, want %q", path, "/media/external/card")
	}
}

// TestTrayPickDirectoryCanceled pins the cancel half: dialogExitCanceled
// must come back as an error and an empty path so
// handleImportFolder's "if err != nil || path == ”" guard
// (internal/tray/importfolder.go) aborts cleanly.
func TestTrayPickDirectoryCanceled(t *testing.T) {
	run := &fakeDialogRunner{exitCode: dialogExitCanceled}
	pick := trayPickDirectory(run.Run)
	path, err := pick(context.Background(), "Import from folder…")
	if err == nil {
		t.Fatal("expected error on canceled directory picker")
	}
	if path != "" {
		t.Errorf("got path %q, want empty", path)
	}
}

// TestTrayPickDirectoryFailedRender covers the "dialog did not render"
// half: both an unexpected nonzero exit and a subprocess error must
// surface as a non-nil error so the import-folder worker aborts
// instead of ingesting "" or the unparsed stdout.
func TestTrayPickDirectoryFailedRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  *fakeDialogRunner
	}{
		{"unexpected nonzero exit", &fakeDialogRunner{exitCode: 99}},
		{"subprocess error", &fakeDialogRunner{err: errors.New("no display")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pick := trayPickDirectory(tc.run.Run)
			_, err := pick(context.Background(), "Import from folder…")
			if err == nil {
				t.Fatal("expected error on failed directory picker")
			}
		})
	}
}

// TestTrayPickDirectoryForwardsTitle proves the title actually threads
// into the re-exec'd `dialog` argv -- a swap/swap/drop bug at the call
// site would render a blank picker and pass every other assertion.
func TestTrayPickDirectoryForwardsTitle(t *testing.T) {
	run := &fakeDialogRunner{stdout: "/foo", exitCode: dialogExitOK}
	pick := trayPickDirectory(run.Run)
	_, _ = pick(context.Background(), "Import from folder…")

	if len(run.calls) != 1 {
		t.Fatalf("expected exactly one dialog call, got %d", len(run.calls))
	}
	args := run.calls[0]
	seen := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		seen[args[i]] = args[i+1]
	}
	if got := seen["-kind"]; got != "directory" {
		t.Errorf("got -kind %q, want %q", got, "directory")
	}
	if got := seen["-title"]; got != "Import from folder…" {
		t.Errorf("got -title %q, want %q", got, "Import from folder…")
	}
}

// TestTrayNotifyOSForwardsTitleAndMessage proves the notify callback
// threads both title and message into the re-exec'd `dialog -kind
// notify` argv -- the notify path has only one production caller
// (handleImportFolder's "Already ingesting this path") and a wiring
// bug here would silently render a no-op toast.
func TestTrayNotifyOSForwardsTitleAndMessage(t *testing.T) {
	run := &fakeDialogRunner{exitCode: dialogExitOK}
	notify := trayNotifyOS(run.Run)
	notify(context.Background(), "branchDAM Agent", "Already ingesting this path")

	if len(run.calls) != 1 {
		t.Fatalf("expected exactly one dialog call, got %d", len(run.calls))
	}
	args := run.calls[0]
	seen := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		seen[args[i]] = args[i+1]
	}
	if got := seen["-kind"]; got != "notify" {
		t.Errorf("got -kind %q, want %q", got, "notify")
	}
	if got := seen["-title"]; got != "branchDAM Agent" {
		t.Errorf("got -title %q, want %q", got, "branchDAM Agent")
	}
	if got := seen["-message"]; got != "Already ingesting this path" {
		t.Errorf("got -message %q, want %q", got, "Already ingesting this path")
	}
}
