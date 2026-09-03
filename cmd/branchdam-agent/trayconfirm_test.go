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

func TestTrayIngestGateAutoImportPathsBypassesDialog(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := &fakeDialogRunner{exitCode: dialogExitCanceled}
	s := newConfigSettings(path, cfg, runner, dialog.Run)

	// Persist autoImportPaths
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

	// TriggerDetectedIngest through the runner with this gate returns the error and does NOT mark the volume skipped
	runner.SetIngestGate(gate)
	summary := runner.TriggerDetectedIngest(context.Background(), "/media/card/CANON_R5")
	if summary.Err == nil {
		t.Fatal("expected TriggerDetectedIngest to return error when gate fails")
	}
	if runner.IsSkipped("/media/card/CANON_R5") {
		t.Fatal("expected volume NOT to be marked skipped after transient dialog failure")
	}

	// Manual TriggerIngest bypasses gate even when gate has error
	manualSummary := runner.TriggerIngest(context.Background(), "/media/card/CANON_R5")
	if !manualSummary.OK() {
		t.Fatalf("expected manual TriggerIngest to proceed unconditionally, got %+v", manualSummary)
	}
}
