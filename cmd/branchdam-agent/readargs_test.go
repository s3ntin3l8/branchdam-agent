package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadArgsFromSidecarRejectsZeroArgs pins the arity guard: -read-args
// with no path is a usage error, exit 2.
func TestReadArgsFromSidecarRejectsZeroArgs(t *testing.T) {
	_, code := readArgsFromSidecar(nil)
	if code != 2 {
		t.Errorf("zero args: got code=%d, want 2", code)
	}
}

// TestReadArgsFromSidecarRejectsTooManyArgs pins the arity guard:
// -read-args with more than one path is a usage error, exit 2. The
// autostart mechanism can never legitimately produce more than one
// path (the plist/reg value is a single <string>); seeing >1 is
// either a caller mistake or a hostile caller, and either way it's
// a usage error.
func TestReadArgsFromSidecarRejectsTooManyArgs(t *testing.T) {
	_, code := readArgsFromSidecar([]string{"a", "b"})
	if code != 2 {
		t.Errorf("two args: got code=%d, want 2", code)
	}
}

// TestReadArgsFromSidecarMissingFile: a non-existent sidecar is a
// real I/O error, exit 1 (not exit 2 -- the call is well-formed,
// the file just isn't there).
func TestReadArgsFromSidecarMissingFile(t *testing.T) {
	_, code := readArgsFromSidecar([]string{"/nonexistent/path/that/does/not/exist.json"})
	if code != 1 {
		t.Errorf("missing file: got code=%d, want 1", code)
	}
}

// TestReadArgsFromSidecarBadJSON: a sidecar with malformed JSON is a
// parse error, exit 1. This is the security-critical path: a
// malicious or partially-written sidecar must be rejected before
// anything tries to use its contents.
func TestReadArgsFromSidecarBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "args.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, code := readArgsFromSidecar([]string{path})
	if code != 1 {
		t.Errorf("bad JSON: got code=%d, want 1", code)
	}
}

// TestReadArgsFromSidecarNotAnArray: a sidecar containing a valid
// JSON value that is NOT an array (e.g. an object, a string) is a
// parse error -- json.Unmarshal into []string fails on a non-array.
// This is the second half of the security check: even if the JSON
// is valid, a non-array shape must be rejected.
func TestReadArgsFromSidecarNotAnArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "args.json")
	if err := os.WriteFile(path, []byte(`{"not":"an array"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, code := readArgsFromSidecar([]string{path})
	if code != 1 {
		t.Errorf("non-array JSON: got code=%d, want 1", code)
	}
}

// TestReadArgsFromSidecarHappyPath: a well-formed sidecar returns
// the parsed args with code 0. This is the primary success path;
// every caller (the tray, -read-args in either platform) reaches it
// at autostart time.
func TestReadArgsFromSidecarHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "args.json")
	want := []string{"tray", "-config", "/etc/config.yaml", "-path", "/foo;calc.exe"}
	data := []byte(`["tray","-config","/etc/config.yaml","-path","/foo;calc.exe"]`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, code := readArgsFromSidecar([]string{path})
	if code != 0 {
		t.Errorf("happy path: got code=%d, want 0", code)
	}
	if len(got) != len(want) {
		t.Fatalf("happy path: got %d args, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("happy path: arg[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReadArgsFromSidecarPreservesShellMetachars verifies that the
// parse path itself does not interpret shell metacharacters -- it
// passes them through verbatim, and the metacharacter handling is
// the caller's responsibility (the original injection fix's whole
// point: the sidecar carries the raw args, never re-quoted).
func TestReadArgsFromSidecarPreservesShellMetachars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "args.json")
	data := []byte(`["tray","-config","C:\\Users\\test;calc.exe","-other","arg>with>special","&another"]`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, code := readArgsFromSidecar([]string{path})
	if code != 0 {
		t.Errorf("metachars: got code=%d, want 0", code)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{";calc.exe", ">with>special", "&another"} {
		if !strings.Contains(joined, want) {
			t.Errorf("metachars: output missing %q (got: %q)", want, joined)
		}
	}
}
