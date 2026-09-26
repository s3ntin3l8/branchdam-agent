// Package protocolhandler keeps the per-user branchdam:// URL protocol
// registration current on Windows. The NSIS installer writes it at install
// time, but a self-update only replaces the executables, so an install that
// predates the registration would otherwise keep a dead "Pair with local
// agent" button until reinstalled. The tray calls Register at startup, the
// same place it refreshes its autostart entry. Elsewhere it is a no-op:
// macOS registers the scheme through the bundle's Info.plist.
package protocolhandler

import (
	"os"
	"path/filepath"
	"strings"
)

// Command is the registered shell\open\command for the tray executable at
// execPath. It names `pair -deeplink` explicitly rather than passing a bare
// "%1", so a build that predates deep-link confirmation rejects the unknown
// flag and writes nothing (see cmd/branchdam-agent's runPairCmd).
func Command(execPath string) string {
	return `"` + strings.ReplaceAll(execPath, `"`, `\"`) + `" pair -deeplink "%1"`
}

// HandlerExe picks the executable the protocol should launch: the GUI-subsystem
// branchdam-agent-tray.exe next to self when it exists (no console flash),
// otherwise self.
func HandlerExe(self string) string {
	tray := filepath.Join(filepath.Dir(self), "branchdam-agent-tray.exe")
	if _, err := os.Stat(tray); err == nil {
		return tray
	}
	return self
}
