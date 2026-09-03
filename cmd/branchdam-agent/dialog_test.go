package main

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/ncruces/zenity"
)

// captureStdout redirects os.Stdout for the duration of fn, returning
// whatever was written -- runDialogCmd's success contract for
// entry/password/directory is "print the value to stdout," which fmt.Println
// writes directly to the package-level os.Stdout, not an injectable writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

func fakeDialogFuncs() dialogFuncs {
	return dialogFuncs{
		showError: func(title, message string) error { return nil },
		entry: func(title, message, defaultText string, hidden bool) (string, error) {
			return "typed-value", nil
		},
		directory: func(title string) (string, error) {
			return "/chosen/dir", nil
		},
		file: func(title, defaultPath string, patterns []string) (string, error) {
			return "/chosen/file.db", nil
		},
		question: func(title, message, okLabel, cancelLabel, extraButton string) error { return nil },
		notify:   func(title, message string) error { return nil },
	}
}

func TestRunDialogCmdError(t *testing.T) {
	dlg := fakeDialogFuncs()
	got := runDialogCmd([]string{"-kind", "error", "-title", "T", "-message", "M"}, dlg)
	if got != dialogExitOK {
		t.Errorf("got exit %d, want %d", got, dialogExitOK)
	}
}

func TestRunDialogCmdErrorFailure(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.showError = func(title, message string) error { return errors.New("no display") }
	got := runDialogCmd([]string{"-kind", "error", "-message", "M"}, dlg)
	if got != dialogExitFailed {
		t.Errorf("got exit %d, want %d", got, dialogExitFailed)
	}
}

func TestRunDialogCmdEntryPrintsValue(t *testing.T) {
	dlg := fakeDialogFuncs()
	var code int
	out := captureStdout(t, func() {
		code = runDialogCmd([]string{"-kind", "entry", "-message", "Enter something:"}, dlg)
	})
	if code != dialogExitOK {
		t.Errorf("got exit %d, want %d", code, dialogExitOK)
	}
	if out != "typed-value\n" {
		t.Errorf("got stdout %q, want %q", out, "typed-value\n")
	}
}

func TestRunDialogCmdPasswordHidesText(t *testing.T) {
	var gotHidden bool
	dlg := fakeDialogFuncs()
	dlg.entry = func(title, message, defaultText string, hidden bool) (string, error) {
		gotHidden = hidden
		return "secret", nil
	}
	captureStdout(t, func() {
		runDialogCmd([]string{"-kind", "password", "-message", "Key:"}, dlg)
	})
	if !gotHidden {
		t.Error("expected -kind password to request a hidden entry")
	}
}

func TestRunDialogCmdEntryCanceled(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.entry = func(title, message, defaultText string, hidden bool) (string, error) {
		return "", zenity.ErrCanceled
	}
	got := runDialogCmd([]string{"-kind", "entry", "-message", "M"}, dlg)
	if got != dialogExitCanceled {
		t.Errorf("got exit %d, want %d (canceled)", got, dialogExitCanceled)
	}
}

func TestRunDialogCmdDirectoryPrintsPath(t *testing.T) {
	dlg := fakeDialogFuncs()
	var code int
	out := captureStdout(t, func() {
		code = runDialogCmd([]string{"-kind", "directory", "-title", "Pick a folder"}, dlg)
	})
	if code != dialogExitOK {
		t.Errorf("got exit %d, want %d", code, dialogExitOK)
	}
	if out != "/chosen/dir\n" {
		t.Errorf("got stdout %q", out)
	}
}

func TestRunDialogCmdFilePrintsPath(t *testing.T) {
	dlg := fakeDialogFuncs()
	var code int
	out := captureStdout(t, func() {
		code = runDialogCmd([]string{"-kind", "file", "-title", "Pick a file"}, dlg)
	})
	if code != dialogExitOK {
		t.Errorf("got exit %d, want %d", code, dialogExitOK)
	}
	if out != "/chosen/file.db\n" {
		t.Errorf("got stdout %q", out)
	}
}

func TestRunDialogCmdFileCanceled(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.file = func(title, defaultPath string, patterns []string) (string, error) {
		return "", zenity.ErrCanceled
	}
	got := runDialogCmd([]string{"-kind", "file"}, dlg)
	if got != dialogExitCanceled {
		t.Errorf("got exit %d, want %d (canceled)", got, dialogExitCanceled)
	}
}

func TestRunDialogCmdFileFailure(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.file = func(title, defaultPath string, patterns []string) (string, error) {
		return "", errors.New("no display")
	}
	got := runDialogCmd([]string{"-kind", "file"}, dlg)
	if got != dialogExitFailed {
		t.Errorf("got exit %d, want %d", got, dialogExitFailed)
	}
}

// TestRunDialogCmdFilePassesDefaultAndPatterns proves -default and
// -patterns actually thread through into dlg.file's arguments, not just
// that -kind file dispatches correctly.
func TestRunDialogCmdFilePassesDefaultAndPatterns(t *testing.T) {
	var gotTitle, gotDefault string
	var gotPatterns []string
	dlg := fakeDialogFuncs()
	dlg.file = func(title, defaultPath string, patterns []string) (string, error) {
		gotTitle, gotDefault, gotPatterns = title, defaultPath, patterns
		return "/x", nil
	}
	captureStdout(t, func() {
		runDialogCmd([]string{
			"-kind", "file",
			"-title", "Select the Luminar catalog",
			"-default", "/data/catalog.db",
			"-patterns", "*.lrcat, *.db ,,*.json",
		}, dlg)
	})
	if gotTitle != "Select the Luminar catalog" {
		t.Errorf("got title %q", gotTitle)
	}
	if gotDefault != "/data/catalog.db" {
		t.Errorf("got default %q", gotDefault)
	}
	wantPatterns := []string{"*.lrcat", "*.db", "*.json"}
	if len(gotPatterns) != len(wantPatterns) {
		t.Fatalf("got patterns %v, want %v", gotPatterns, wantPatterns)
	}
	for i, p := range wantPatterns {
		if gotPatterns[i] != p {
			t.Errorf("pattern[%d] = %q, want %q", i, gotPatterns[i], p)
		}
	}
}

func TestSplitPatternsEmptyAndWhitespaceOnly(t *testing.T) {
	for _, in := range []string{"", "   ", ",", " , , "} {
		if got := splitPatterns(in); got != nil {
			t.Errorf("splitPatterns(%q) = %v, want nil", in, got)
		}
	}
}

// TestRunDialogCmdQuestionOK covers the success half of the new
// -kind question dispatch (issue #108 / E3 #S2-14): dlg.question
// returns nil (operator clicked OK), and runDialogCmd must surface
// that as dialogExitOK so the tray's confirm callback (cmd/branchdam-agent/tray.go's
// trayConfirm) maps it to "proceed with the destructive action."
func TestRunDialogCmdQuestionOK(t *testing.T) {
	dlg := fakeDialogFuncs()
	got := runDialogCmd([]string{"-kind", "question", "-title", "Confirm", "-message", "Are you sure?"}, dlg)
	if got != dialogExitOK {
		t.Errorf("got exit %d, want %d", got, dialogExitOK)
	}
}

// TestRunDialogCmdQuestionCanceled pins the cancel half: dlg.question
// returns zenity.ErrCanceled (the standard "Cancel/window-close" sentinel
// from github.com/ncruces/zenity), and runDialogCmd must surface that as
// dialogExitCanceled so the tray's confirm callback refuses the action
// without logging it as a render failure.
func TestRunDialogCmdQuestionCanceled(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.question = func(title, message, okLabel, cancelLabel, extraButton string) error { return zenity.ErrCanceled }
	got := runDialogCmd([]string{"-kind", "question", "-title", "T", "-message", "M"}, dlg)
	if got != dialogExitCanceled {
		t.Errorf("got exit %d, want %d (canceled)", got, dialogExitCanceled)
	}
}

// TestRunDialogCmdQuestionExtraButton tests that ErrExtraButton is mapped to dialogExitExtraButton.
func TestRunDialogCmdQuestionExtraButton(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.question = func(title, message, okLabel, cancelLabel, extraButton string) error { return zenity.ErrExtraButton }
	got := runDialogCmd([]string{
		"-kind", "question",
		"-title", "New volume detected",
		"-message", "New volume detected: CANON R5 (32 GB)",
		"-ok-label", "Import",
		"-cancel-label", "Skip this time",
		"-extra-button", "Always auto-import",
	}, dlg)
	if got != dialogExitExtraButton {
		t.Errorf("got exit %d, want %d (extra button)", got, dialogExitExtraButton)
	}
}

// TestRunDialogCmdQuestionFailure pins the "dialog did not even render"
// branch: a question dialog that returns any non-ErrCanceled error
// (the subprocess failed to spawn, no display, zenity backend missing)
// must come back as dialogExitFailed. The tray's confirm callback
// treats that the same as Cancel -- refuse the action -- but a
// different exit code lets the callback's slog line (if any caller
// surfaces one) distinguish "operator said no" from "this host
// can't show a dialog."
func TestRunDialogCmdQuestionFailure(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.question = func(title, message, okLabel, cancelLabel, extraButton string) error { return errors.New("no display") }
	got := runDialogCmd([]string{"-kind", "question", "-title", "T", "-message", "M"}, dlg)
	if got != dialogExitFailed {
		t.Errorf("got exit %d, want %d (failed)", got, dialogExitFailed)
	}
}

// TestRunDialogCmdQuestionPassesTitleAndMessage proves the new -kind
// question actually threads its -title and -message into dlg.question,
// not just that it dispatches by kind. Without this, a wiring bug that
// swapped title/message at the call site (or dropped -title) would
// silently render a blank dialog and stay green.
func TestRunDialogCmdQuestionPassesTitleAndMessage(t *testing.T) {
	var gotTitle, gotMessage, gotOK, gotCancel, gotExtra string
	dlg := fakeDialogFuncs()
	dlg.question = func(title, message, okLabel, cancelLabel, extraButton string) error {
		gotTitle, gotMessage, gotOK, gotCancel, gotExtra = title, message, okLabel, cancelLabel, extraButton
		return nil
	}
	runDialogCmd([]string{
		"-kind", "question",
		"-title", "Confirm prune",
		"-message", "Delete files?",
		"-ok-label", "Yes",
		"-cancel-label", "No",
		"-extra-button", "Always",
	}, dlg)
	if gotTitle != "Confirm prune" {
		t.Errorf("got title %q, want %q", gotTitle, "Confirm prune")
	}
	if gotMessage != "Delete files?" {
		t.Errorf("got message %q, want %q", gotMessage, "Delete files?")
	}
	if gotOK != "Yes" {
		t.Errorf("got ok-label %q, want %q", gotOK, "Yes")
	}
	if gotCancel != "No" {
		t.Errorf("got cancel-label %q, want %q", gotCancel, "No")
	}
	if gotExtra != "Always" {
		t.Errorf("got extra-button %q, want %q", gotExtra, "Always")
	}
}

func TestRunDialogCmdNotify(t *testing.T) {
	var gotTitle, gotMessage string
	dlg := fakeDialogFuncs()
	dlg.notify = func(title, message string) error {
		gotTitle, gotMessage = title, message
		return nil
	}
	got := runDialogCmd([]string{"-kind", "notify", "-title", "branchDAM Agent", "-message", "8 photos imported from CANON R5"}, dlg)
	if got != dialogExitOK {
		t.Errorf("got exit %d, want %d", got, dialogExitOK)
	}
	if gotTitle != "branchDAM Agent" {
		t.Errorf("got title %q, want %q", gotTitle, "branchDAM Agent")
	}
	if gotMessage != "8 photos imported from CANON R5" {
		t.Errorf("got message %q, want %q", gotMessage, "8 photos imported from CANON R5")
	}
}

func TestRunDialogCmdNotifyFailure(t *testing.T) {
	dlg := fakeDialogFuncs()
	dlg.notify = func(title, message string) error { return errors.New("notify failed") }
	got := runDialogCmd([]string{"-kind", "notify", "-title", "T", "-message", "M"}, dlg)
	if got != dialogExitFailed {
		t.Errorf("got exit %d, want %d", got, dialogExitFailed)
	}
}

func TestRunDialogCmdUnknownKind(t *testing.T) {
	dlg := fakeDialogFuncs()
	got := runDialogCmd([]string{"-kind", "bogus"}, dlg)
	if got != 2 {
		t.Errorf("got exit %d, want 2", got)
	}
}

func TestRunDialogCmdMissingKind(t *testing.T) {
	dlg := fakeDialogFuncs()
	got := runDialogCmd([]string{}, dlg)
	if got != 2 {
		t.Errorf("got exit %d, want 2", got)
	}
}

func TestRunDialogCmdBadFlag(t *testing.T) {
	dlg := fakeDialogFuncs()
	got := runDialogCmd([]string{"-nope"}, dlg)
	if got != 2 {
		t.Errorf("got exit %d, want 2", got)
	}
}
