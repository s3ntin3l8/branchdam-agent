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
		w.Close()

		outR, outW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		got := confirm(r, outW, "prompt: ")
		outW.Close()
		outR.Close()
		r.Close()

		if got != want {
			t.Errorf("confirm(%q) = %v, want %v", input, got, want)
		}
	}
}
