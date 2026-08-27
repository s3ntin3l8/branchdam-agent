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
