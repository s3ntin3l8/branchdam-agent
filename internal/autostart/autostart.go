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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// SidecarPath returns the path to the JSON sidecar file that stores
// the args array for the autostart command. The sidecar is written to
// the per-user config directory alongside config.yaml.
func SidecarPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("autostart: resolve config directory: %w", err)
	}
	return filepath.Join(dir, "branchdam-agent", "args.json"), nil
}

// WriteSidecar writes args as a JSON array to the sidecar file, creating
// the parent directory if needed. The sidecar is read back by the
// -read-args subcommand at login time.
//
// The write is atomic: a temp file is created in the same directory and
// renamed over the final path via os.Rename. A kill mid-write leaves the
// previous valid sidecar in place (or no sidecar on a first run) rather
// than a truncated/invalid file that would silently disable autostart at
// the next login.
func WriteSidecar(args []string) error {
	path, err := SidecarPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("autostart: create sidecar directory: %w", err)
	}
	data, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("autostart: marshal args: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "args.json.tmp.*")
	if err != nil {
		return fmt.Errorf("autostart: create temp sidecar: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("autostart: write temp sidecar: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("autostart: chmod temp sidecar: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("autostart: fsync temp sidecar: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("autostart: close temp sidecar: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("autostart: rename temp sidecar: %w", err)
	}
	return nil
}

// RemoveSidecar removes the args sidecar file. A missing file is not an error.
func RemoveSidecar() {
	path, err := SidecarPath()
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// RenderLaunchAgentPlistReadArgs renders the LaunchAgent plist XML that starts
// execPath with -read-args sidecarPath at login. This avoids embedding
// potentially dangerous characters in the plist command arguments.
func RenderLaunchAgentPlistReadArgs(execPath string, sidecarPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-read-args</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <false/>
</dict>
</plist>
`, xmlEscape(Label), xmlEscape(execPath), xmlEscape(sidecarPath))
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
