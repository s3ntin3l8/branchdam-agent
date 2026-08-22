// Package autostart registers (or removes) a per-user login item so the
// tray starts automatically at login: a LaunchAgent plist on macOS, a
// Run-key value on Windows. Gated behind config.TrayConfig.StartOnLogin,
// off by default -- see issue #3's scope. This file holds only pure,
// platform-independent rendering logic (XML/string building) so it is
// unit-testable on any host, including Linux CI, without touching a real
// filesystem location or registry key; the actual file/registry writes
// live in autostart_darwin.go and autostart_windows.go (each gated by its
// own build tag), with autostart_other.go providing a no-op-with-error
// stub everywhere else.
package autostart

import (
	"errors"
	"fmt"
	"strings"
)

// Label is the reverse-DNS-style identifier used for both the macOS
// LaunchAgent's <key>Label</key> and its plist filename
// (~/Library/LaunchAgents/<Label>.plist).
const Label = "com.branchdam.agent"

// RunKeyName is the value name written under
// HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run.
const RunKeyName = "BranchDAMAgent"

// ErrUnsupported is returned by Enable/Disable on any platform other than
// windows/darwin -- login-item registration, like the tray itself, is
// scoped to those two per the plan doc.
var ErrUnsupported = errors.New("autostart: unsupported on this platform (windows and darwin only)")

// RenderLaunchAgentPlist renders the LaunchAgent plist XML that starts
// execPath with args at login, RunAtLoad but not KeepAlive (a crashed tray
// should not be relentlessly respawned by launchd; the operator's next
// login restarts it same as any other login item). Pure string building,
// no file I/O -- see autostart_darwin.go for the write.
func RenderLaunchAgentPlist(execPath string, args []string) string {
	var argXML strings.Builder
	fmt.Fprintf(&argXML, "        <string>%s</string>\n", xmlEscape(execPath))
	for _, a := range args {
		fmt.Fprintf(&argXML, "        <string>%s</string>\n", xmlEscape(a))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <false/>
</dict>
</plist>
`, xmlEscape(Label), argXML.String())
}

// LaunchAgentRelPath is the plist's path relative to the user's home
// directory: ~/Library/LaunchAgents/<Label>.plist.
func LaunchAgentRelPath() string {
	return "Library/LaunchAgents/" + Label + ".plist"
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
