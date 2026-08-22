package autostart

import (
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
