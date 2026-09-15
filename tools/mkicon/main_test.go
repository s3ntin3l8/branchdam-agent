package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRequiresArg(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
	if got := run([]string{""}); got != 2 {
		t.Errorf(`run([""]) = %d, want 2`, got)
	}
}

func TestRunWritesICO(t *testing.T) {
	out := filepath.Join(t.TempDir(), "icon.ico")

	if got := run([]string{out}); got != 0 {
		t.Fatalf("run() = %d, want 0", got)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("output not written: %v", err)
	}
	if !bytes.HasPrefix(data, []byte{0, 0, 1, 0}) {
		t.Errorf("output does not start with an ICONDIR header, got %v", data[:min(4, len(data))])
	}
}
