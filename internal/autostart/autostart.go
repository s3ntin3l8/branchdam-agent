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
//
// Returns the final sidecar path on success so the caller (Enable on
// darwin and windows) can pass it to RenderLaunchAgentPlistReadArgs / the
// Run-key without a redundant SidecarPath() call. On error, the path
// is not meaningful.
func WriteSidecar(args []string) (string, error) {
	path, err := SidecarPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("autostart: create sidecar directory: %w", err)
	}
	data, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("autostart: marshal args: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "args.json.tmp.*")
	if err != nil {
		return "", fmt.Errorf("autostart: create temp sidecar: %w", err)
	}
	tmpPath := tmp.Name()
	// closeTmpAndWrap closes tmp and returns an error joining the
	// original op error (origErr) with any close error so a close
	// failure on a write-failure path is not silently lost. The temp
	// file is also removed by the defer so the rename below doesn't
	// trip over a half-written file.
	closeTmpAndWrap := func(origErr error) error {
		cerr := tmp.Close()
		if cerr == nil {
			return origErr
		}
		if origErr == nil {
			return fmt.Errorf("autostart: close temp sidecar: %w", cerr)
		}
		return fmt.Errorf("autostart: %w (close: %v)", origErr, cerr)
	}
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return "", fmt.Errorf("autostart: write temp sidecar: %w", closeTmpAndWrap(err))
	}
	if err := tmp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("autostart: chmod temp sidecar: %w", closeTmpAndWrap(err))
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("autostart: fsync temp sidecar: %w", closeTmpAndWrap(err))
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("autostart: close temp sidecar: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("autostart: rename temp sidecar: %w", err)
	}
	return path, nil
}

// RemoveSidecar removes the args sidecar file and, if it is now empty,
// the parent config directory too. A missing file is not an error; a
// non-empty parent directory is also not an error (something else --
// the future config.yaml the user is editing, for example -- lives
// there and must not be deleted). The empty-dir cleanup is best-effort
// and does its work only when no other file remains in the directory.
func RemoveSidecar() {
	path, err := SidecarPath()
	if err != nil {
		return
	}
	_ = os.Remove(path)
	// Best-effort empty-dir cleanup: read the directory; if it now
	// contains zero entries, rmdir it. A non-empty result means
	// something else lives there (a config.yaml, etc.) -- leave it.
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(dir)
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
