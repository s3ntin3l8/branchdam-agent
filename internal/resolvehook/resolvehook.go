// Package resolvehook detects and installs a script hook into DaVinci
// Resolve's Scripts/Utility directory -- DaVinci Resolve's render-hook
// installer (issue #60), the first (and so far only) consumer. Knows
// nothing about hooks/resolve's embedded script bytes or
// cmd/branchdam-agent's tray wiring: callers pass in the file name,
// expected checksum, and source bytes explicitly, keeping this package a
// reusable, dependency-free filesystem operation -- the same layering
// internal/luminar has relative to cmd/branchdam-agent's own wiring.
package resolvehook

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// HookState is this package's own filesystem-state value type --
// deliberately NOT tray.HookState (which adds an At timestamp and an Err
// field that only make sense once a caller has decided WHEN this was
// computed and whether the call itself failed). cmd/branchdam-agent's own
// wiring converts between the two, mirroring how luminarSyncer.Sync
// converts luminar.Stats into tray.SyncSummary.
type HookState struct {
	// Dir is the directory scanned (Detect) or written into (Install).
	// Empty only when Detect found no candidate directory at all -- the
	// caller renders that as "no Scripts folder found," never as "not
	// installed."
	Dir string
	// Path is Dir/FileName, set whenever Installed is true.
	Path      string
	Installed bool
	// UpToDate is only meaningful when Installed is true. Installed &&
	// !UpToDate covers BOTH "hand-edited" and "an older shipped version"
	// -- both are the same SHA-256 mismatch, indistinguishable by design;
	// the caller's own status line says "modified or out of date" rather
	// than guessing which.
	UpToDate bool
}

// CandidateDirs returns Resolve's Scripts/Utility directories in
// PREFERENCE order for this OS, most-writable first. home/appData are
// injected (rather than read from os.UserHomeDir/os.Getenv here) so this
// is table-testable on any host, including this repo's own Linux
// dev/CI machine, without touching the real filesystem or environment.
//
// macOS deserves a note: hooks/resolve/README.md's own install
// instructions list only the SYSTEM-WIDE
// "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility"
// path, which an unprivileged tray cannot write without admin rights --
// installing there would fail with EACCES and look like a bug in this
// package rather than an OS permission fact. Resolve also reads the
// PER-USER "~/Library/Application Support/..." tree, so that one is
// listed FIRST here (the install target) and the system path second
// (detect-only, in case an operator installed it there by hand or via an
// admin-run process).
func CandidateDirs(goos, home, appData string) []string {
	switch goos {
	case "windows":
		if appData == "" {
			return nil
		}
		return []string{filepath.Join(appData, "Blackmagic Design", "DaVinci Resolve", "Support", "Fusion", "Scripts", "Utility")}
	case "darwin":
		var dirs []string
		if home != "" {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "Blackmagic Design", "DaVinci Resolve", "Fusion", "Scripts", "Utility"))
		}
		dirs = append(dirs, "/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility")
		return dirs
	default:
		// The tray doesn't run on Linux (internal/tray/run_unsupported.go),
		// but this package must still compile and be testable there --
		// and the path itself is a real, documented Resolve install
		// location (hooks/resolve/README.md), detect-only since it's
		// typically root-owned.
		return []string{"/opt/resolve/Fusion/Scripts/Utility"}
	}
}

// Detect scans dirs in order for a file named fileName. The first
// directory that already contains one wins for status, its checksum
// compared against wantSHA256 to set UpToDate; if none does, the first
// EXISTING directory in dirs is reported as the install target with
// Installed=false. An empty dirs, or one where no directory exists at
// all, reports a zero-value HookState (Dir == ""). Never errors -- a
// permission-denied stat is treated the same as "not installed there,"
// which is the honest outcome either way (the file isn't usable at that
// candidate regardless of why).
func Detect(dirs []string, fileName, wantSHA256 string) HookState {
	firstExisting := ""
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		if firstExisting == "" {
			firstExisting = dir
		}
		path := filepath.Join(dir, fileName)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		return HookState{
			Dir:       dir,
			Path:      path,
			Installed: true,
			UpToDate:  hex.EncodeToString(sum[:]) == wantSHA256,
		}
	}
	if firstExisting == "" {
		return HookState{}
	}
	return HookState{Dir: firstExisting, Installed: false}
}

// Install atomically writes source into dir/fileName -- a temp file in
// the SAME directory, then rename, so a crash mid-write can never leave
// Resolve a truncated, syntactically-invalid Python file it will fail to
// import (the same shape as internal/config/patch.go's own
// writeFileAtomic, duplicated rather than exported across the
// internal-package boundary for a ~15-line function, matching
// cmd/branchdam-agent/settings.go's openWithDefaultApp precedent against
// internal/tray's own openBrowser). Mode 0644, not config.Patch's 0600 --
// this is a script Resolve reads, not a secret. Creates dir if it
// doesn't exist yet (0755), since CandidateDirs' own install-target
// entry may not exist until the very first install.
func Install(dir, fileName string, source []byte) (HookState, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return HookState{}, fmt.Errorf("resolvehook: create %s: %w", dir, err)
	}

	path := filepath.Join(dir, fileName)
	tmp, err := os.CreateTemp(dir, fileName+".tmp-*")
	if err != nil {
		return HookState{}, fmt.Errorf("resolvehook: create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename --
	// a leftover .tmp-* file is harmless clutter, never mistaken for the
	// real hook (Resolve's Scripts menu lists by the exact fileName).
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(source); err != nil {
		_ = tmp.Close()
		return HookState{}, fmt.Errorf("resolvehook: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return HookState{}, fmt.Errorf("resolvehook: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return HookState{}, fmt.Errorf("resolvehook: close %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return HookState{}, fmt.Errorf("resolvehook: chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return HookState{}, fmt.Errorf("resolvehook: rename %s to %s: %w", tmpPath, path, err)
	}

	return HookState{Dir: dir, Path: path, Installed: true, UpToDate: true}, nil
}
