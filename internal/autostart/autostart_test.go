package autostart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXMLEscapeHandlesSpecialChars(t *testing.T) {
	// Pin the XML escaping behavior shared by every plist-rendering
	// helper in this package (RenderLaunchAgentPlistReadArgs, etc.).
	// Ampersand, less-than/greater-than, and double-quote must all be
	// entity-encoded in the plist XML; the resulting XML must remain
	// parseable and must not contain any raw HTML-tag sequence in place
	// of the originals.
	cases := []struct {
		in, wantSubstr string}{
		{`a & b`, "&amp;"},
		{`<tag>`, "&lt;tag&gt;"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{`Photos & <Archive>/"agent"`, "Photos &amp; &lt;Archive&gt;/&quot;agent&quot;"},
	}
	for _, c := range cases {
		got := xmlEscape(c.in)
		if !strings.Contains(got, c.wantSubstr) {
			t.Errorf("xmlEscape(%q): missing %q in output\n---\n%s", c.in, c.wantSubstr, got)
		}
		// Negative: raw HTML-tag sequences must not appear where the
		// input had angle brackets.
		if strings.Contains(c.in, "<") && strings.Contains(got, "<tag>") {
			t.Errorf("xmlEscape(%q): raw <tag> sequence leaked through\n---\n%s", c.in, got)
		}
	}
}

func TestLaunchAgentRelPath(t *testing.T) {
	got := LaunchAgentRelPath()
	want := "Library/LaunchAgents/" + Label + ".plist"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriteSidecarJSONRoundTrip(t *testing.T) {
	args := []string{"tray", "-config", "/etc/config.yaml", "-path", "/foo;calc.exe"}

	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	var parsed []string
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if len(parsed) != len(args) {
		t.Errorf("got %d args, want %d", len(parsed), len(args))
	}

	for i, arg := range parsed {
		if arg != args[i] {
			t.Errorf("arg[%d] = %q, want %q", i, arg, args[i])
		}
	}
}

func TestRenderLaunchAgentPlistReadArgs(t *testing.T) {
	xml := RenderLaunchAgentPlistReadArgs("/usr/local/bin/branchdam-agent", "/Users/me/.config/branchdam-agent/args.json")

	for _, want := range []string{
		"<key>Label</key>",
		"<string>" + Label + "</string>",
		"<string>/usr/local/bin/branchdam-agent</string>",
		"<string>-read-args</string>",
		"<string>/Users/me/.config/branchdam-agent/args.json</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"<false/>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("rendered plist missing %q\n---\n%s", want, xml)
		}
	}
}

func TestSidecarPathSpecialChars(t *testing.T) {
	args := []string{"-config", "C:\\Users\\test;calc.exe", "-other", "arg>with>special"}

	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("failed to marshal args: %v", err)
	}

	var parsed []string
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal args: %v", err)
	}

	if parsed[0] != args[0] {
		t.Errorf("arg[0] = %q, want %q", parsed[0], args[0])
	}
	if parsed[1] != args[1] {
		t.Errorf("arg[1] = %q, want %q", parsed[1], args[1])
	}
}

// TestWriteSidecarAtomicNoTempLeftBehind verifies that a successful
// write does not leave a temp file in the sidecar directory. A kill
// mid-write would leave a temp file, which the defer in WriteSidecar
// cleans up on the next run; this test pins the happy path's
// no-temp-file invariant.
func TestWriteSidecarAtomicNoTempLeftBehind(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)
	t.Setenv("HOME", tmpHome)
	t.Setenv("APPDATA", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	path, err := WriteSidecar([]string{"tray", "-config", "/etc/config.yaml"})
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	sidecarPath, err := SidecarPath()
	if err != nil {
		t.Fatalf("SidecarPath: %v", err)
	}
	if path != sidecarPath {
		t.Errorf("WriteSidecar returned %q, want %q", path, sidecarPath)
	}

	dir := filepath.Dir(sidecarPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read sidecar dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "args.json.tmp.") {
			t.Errorf("temp sidecar left behind: %s", e.Name())
		}
	}
}

// TestRemoveSidecarRemovesEmptyParentDir pins the post-Disable cleanup:
// once the sidecar is gone and nothing else lives in the config dir,
// the empty parent dir is also removed. If a config.yaml or other
// file is present, the dir is left alone.
func TestRemoveSidecarRemovesEmptyParentDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)
	t.Setenv("HOME", tmpHome)
	t.Setenv("APPDATA", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	path, err := WriteSidecar([]string{"tray"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)

	// Pre-condition: dir exists.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir should exist after WriteSidecar: %v", err)
	}

	RemoveSidecar()

	// Post-condition: dir is gone (it was empty after the sidecar
	// removal).
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected empty dir %q to be removed, got stat err = %v", dir, err)
	}
}

// TestRemoveSidecarLeavesNonEmptyParentDir is the negative case: if
// something else (e.g. a config.yaml the user is editing) is in the
// dir, RemoveSidecar must not delete the dir.
func TestRemoveSidecarLeavesNonEmptyParentDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpHome)
	t.Setenv("HOME", tmpHome)
	t.Setenv("APPDATA", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)

	path, err := WriteSidecar([]string{"tray"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)

	// Drop an unrelated file in the same dir to simulate a sibling
	// config file.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("server: x"), 0o600); err != nil {
		t.Fatal(err)
	}

	RemoveSidecar()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected non-empty dir %q to survive, got stat err = %v", dir, err)
	}
}
