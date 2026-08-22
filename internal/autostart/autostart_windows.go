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

// Enable writes a Run-key value that launches execPath with args at login.
// Each argument is individually quoted so a path containing spaces (the
// common case on Windows, e.g. "C:\Program Files\...") round-trips through
// cmd's own command-line parsing correctly.
func Enable(execPath string, args []string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()

	cmd := quoteArg(execPath)
	for _, a := range args {
		cmd += " " + quoteArg(a)
	}

	if err := k.SetStringValue(RunKeyName, cmd); err != nil {
		return fmt.Errorf("autostart: set Run key value: %w", err)
	}
	return nil
}

// Disable removes the Run-key value. Deleting an already-absent value is
// not an error -- the goal state ("not launched at login") already holds.
func Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open Run key: %w", err)
	}
	defer k.Close()

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
