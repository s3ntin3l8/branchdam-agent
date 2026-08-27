package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

func TestSelfDialogRunnerEmptyExeFailsFast(t *testing.T) {
	run := selfDialogRunner("")
	_, _, err := run("-kind", "error")
	if err == nil {
		t.Error("expected an error when selfExe is empty")
	}
}

func TestNotifyStartupFailureNeverPanicsOnRunError(t *testing.T) {
	var gotArgs []string
	run := func(args ...string) (string, int, error) {
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
	run := func(args ...string) (string, int, error) {
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
		"server.baseUrl":       "https://branchdam.example.com",
		"server.apiKey":        "0123456789abcdef0123456789abcdef",
		"ingest.archiveRoot":   "/archive",
		"ingest.localEditRoot": "/edit",
	}
	var promptedKinds []string
	run := func(args ...string) (string, int, error) {
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
}

func TestBootstrapConfigInteractiveCanceledMidway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	calls := 0
	run := func(args ...string) (string, int, error) {
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

	run := func(args ...string) (string, int, error) {
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

	run := func(args ...string) (string, int, error) {
		return "", -1, errors.New("exec failed")
	}

	if err := bootstrapConfigInteractive(run, path); err == nil {
		t.Fatal("expected an error when the dialog runner itself fails")
	}
}
