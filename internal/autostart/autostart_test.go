package autostart

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderLaunchAgentPlist(t *testing.T) {
	xml := RenderLaunchAgentPlist("/usr/local/bin/branchdam-agent", []string{"tray", "-config", "/etc/config.yaml"})

	for _, want := range []string{
		"<key>Label</key>",
		"<string>" + Label + "</string>",
		"<string>/usr/local/bin/branchdam-agent</string>",
		"<string>tray</string>",
		"<string>-config</string>",
		"<string>/etc/config.yaml</string>",
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

func TestRenderLaunchAgentPlistEscapesSpecialChars(t *testing.T) {
	xml := RenderLaunchAgentPlist(`/Users/me/Photos & <Archive>/"agent"`, nil)
	if strings.Contains(xml, `& <Archive>`) {
		t.Error("expected XML-unsafe characters to be escaped")
	}
	if !strings.Contains(xml, "&amp;") || !strings.Contains(xml, "&lt;Archive&gt;") || !strings.Contains(xml, "&quot;agent&quot;") {
		t.Errorf("expected escaped entities in output:\n%s", xml)
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
