package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunUpdateRefusesWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "server:\n  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"selfUpdate:\n  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"update", "-config", cfgPath, "-check"})
	if got != 1 {
		t.Errorf("run([update -check]) with selfUpdate.enabled: false = %d, want 1", got)
	}
}

func TestRunUpdateMissingConfigFile(t *testing.T) {
	if got := run([]string{"update", "-config", "/nonexistent/config.yaml"}); got != 1 {
		t.Errorf("run([update]) with missing config = %d, want 1", got)
	}
}

// TestRunUpdateRejectsUnexpectedArgs is a regression test for a Hermes
// review suggestion: an update invocation with stray positional
// arguments (a typo'd flag, a misplaced value) used to be silently
// ignored rather than reported.
func TestRunUpdateRejectsUnexpectedArgs(t *testing.T) {
	if got := run([]string{"update", "unexpected-arg"}); got != 2 {
		t.Errorf("run([update unexpected-arg]) = %d, want 2", got)
	}
}

// TestRunUpdateRollbackNeedsNoConfig proves -rollback is independent of
// selfUpdate.enabled and config entirely (unlike Check/Apply, Rollback
// makes no network call) -- it never even reaches config.ResolvePath, so
// a config path that doesn't exist has no effect on the outcome. The
// real go test binary naturally has no ".previous.version" sidecar next
// to it, so this exercises the "no rollback available" refusal path
// without writing anything near the test binary itself.
func TestRunUpdateRollbackNeedsNoConfig(t *testing.T) {
	got := run([]string{"update", "-rollback", "-yes", "-config", "/nonexistent/config.yaml"})
	if got != 1 {
		t.Errorf("run([update -rollback]) with no previous version available = %d, want 1", got)
	}
}

func TestConfirm(t *testing.T) {
	cases := map[string]bool{
		"y\n":   true,
		"yes\n": true,
		"Y\n":   true,
		"n\n":   false,
		"\n":    false,
		"":      false,
	}
	for input, want := range cases {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.WriteString(input)
		_ = w.Close()

		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		got := confirm(r, outW, "prompt: ")
		_ = outW.Close()
		_ = outR.Close()
		_ = r.Close()

		if got != want {
			t.Errorf("confirm(%q) = %v, want %v", input, got, want)
		}
	}
}
