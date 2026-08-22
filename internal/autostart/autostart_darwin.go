//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Enable writes the LaunchAgent plist to
// ~/Library/LaunchAgents/<Label>.plist and asks launchd to load it
// immediately via `launchctl load -w`, so the change takes effect without
// requiring the operator to log out and back in. The launchctl call is
// best-effort: no macOS host was available to verify its exact behavior
// (see this PR's body / README for the still-open Dock-icon question this
// shares a root cause with) -- a failure there is returned, not silently
// swallowed, but the plist file itself is the durable part of "enabled."
func Enable(execPath string, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: resolve home directory: %w", err)
	}
	plistPath := filepath.Join(home, LaunchAgentRelPath())

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("autostart: create LaunchAgents directory: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(RenderLaunchAgentPlist(execPath, args)), 0o644); err != nil {
		return fmt.Errorf("autostart: write plist: %w", err)
	}

	if err := exec.Command("launchctl", "load", "-w", plistPath).Run(); err != nil {
		return fmt.Errorf("autostart: plist written to %s but launchctl load failed: %w", plistPath, err)
	}
	return nil
}

// Disable unloads the LaunchAgent (best-effort -- a "not loaded" error from
// launchctl is not fatal, since the goal state is "not running at login"
// either way) and removes the plist file.
func Disable() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("autostart: resolve home directory: %w", err)
	}
	plistPath := filepath.Join(home, LaunchAgentRelPath())

	_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()

	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("autostart: remove plist: %w", err)
	}
	return nil
}
