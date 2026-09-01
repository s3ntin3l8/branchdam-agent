//go:build windows

package autostart

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKeyPath is the standard per-user autostart location -- entries here
// run once at login, under the logged-in user's own session (no elevation
// required to read or write it).
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// Enable writes a Run-key value that launches execPath with -read-args
// pointing to a JSON sidecar containing the actual args. This avoids
// embedding potentially dangerous characters (e.g. ;, &, >) in the
// registry value, which cmd.exe would interpret as control metacharacters.
func Enable(execPath string, args []string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()

	sidecar, err := WriteSidecar(args)
	if err != nil {
		return err
	}

	cmd := quoteArg(execPath) + " -read-args " + quoteArg(sidecar)

	if err := k.SetStringValue(RunKeyName, cmd); err != nil {
		return fmt.Errorf("autostart: set Run key value: %w", err)
	}
	return nil
}

// Disable removes the Run-key value and the args sidecar file. Deleting
// an already-absent value is not an error -- the goal state ("not launched
// at login") already holds.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()

	RemoveSidecar()

	if err := k.DeleteValue(RunKeyName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("autostart: delete Run key value: %w", err)
	}
	return nil
}

// quoteArg wraps s in double quotes, escaping any embedded double quote --
// the same convention CreateProcess-family command-line parsing expects.
func quoteArg(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
