// Package agentlog is the agent's one shared logging setup: a durable log
// file plus stderr, installed as slog's process-wide default. It exists
// because, before this package, this repo had no logging infrastructure at
// all beyond ad-hoc fmt.Fprintf(os.Stderr, ...) calls (see cmd/'s many
// "branchdam-agent <subcmd>: ..." lines) -- fine for a console-attached
// subcommand, useless for a `-H windowsgui`-linked tray or a macOS `.app`
// launched by launchd, neither of which has anywhere for stderr to go. See
// issue #30.
//
// Setup is deliberately not called from every subcommand's entry point in
// this package's own tests or from internal/config's tests -- it is wired
// where it matters (the tray and update paths, cmd/branchdam-agent) rather
// than globally, so a bare `go test ./...` run never creates files outside
// a test's own t.TempDir().
package agentlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// maxSizeBytes is the size a log file is allowed to reach before Setup
// rotates it to a ".1" sibling on next open. There is no mid-process
// rotation -- a long-running tray's log simply grows past this until its
// next restart, which is an acceptable trade for not adding a rotation
// dependency to a repo whose CLAUDE.md already prefers a minimal one.
const maxSizeBytes = 5 * 1024 * 1024

// Path returns the platform-appropriate log file location:
//   - Windows: %LOCALAPPDATA%\branchDAM\logs\agent.log
//   - macOS:   ~/Library/Logs/branchDAM/agent.log
//   - other:   $XDG_STATE_HOME/branchdam-agent/agent.log, or
//     ~/.local/state/branchdam-agent/agent.log when XDG_STATE_HOME is unset
func Path() (string, error) {
	return pathForGOOS(runtime.GOOS)
}

// pathForGOOS is Path's logic parameterized on GOOS so all three branches
// are unit-testable from a single host (matching cmd/branchdam-agent's own
// lookPathFunc-style indirection pattern) rather than only ever exercising
// whichever branch the test runner's own OS happens to hit.
func pathForGOOS(goos string) (string, error) {
	switch goos {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("agentlog: %%LOCALAPPDATA%% is not set")
		}
		return filepath.Join(base, "branchDAM", "logs", "agent.log"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agentlog: resolve home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Logs", "branchDAM", "agent.log"), nil
	default:
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "branchdam-agent", "agent.log"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agentlog: resolve home directory: %w", err)
		}
		return filepath.Join(home, ".local", "state", "branchdam-agent", "agent.log"), nil
	}
}

// Setup opens the platform log file (creating its parent directory, and
// rotating an existing file past maxSizeBytes to ".1" first), installs an
// slog.Logger writing to both it and stderr as the process-wide default
// (slog.SetDefault), and returns the resolved path plus a Close func the
// caller must invoke before exiting.
//
// A failure to resolve or open the log file is non-fatal: Setup falls back
// to an stderr-only default logger and still returns the path it *tried*
// to use (possibly empty, if even resolving the path failed) alongside the
// error, since "here's where I tried to log and why that failed" is itself
// useful content for the startup-error surface calling this.
func Setup() (path string, closeFn func() error, err error) {
	noopClose := func() error { return nil }

	path, perr := Path()
	if perr != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return "", noopClose, perr
	}

	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return path, noopClose, fmt.Errorf("agentlog: create log directory: %w", mkErr)
	}

	rotateIfLarge(path)

	f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if ferr != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		return path, noopClose, fmt.Errorf("agentlog: open log file: %w", ferr)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, f), nil)))
	return path, f.Close, nil
}

// rotateIfLarge renames path to path+".1" (best-effort -- a failure here
// just means the file keeps growing, not that logging stops working) when
// it already exceeds maxSizeBytes. Errors from Stat (including "file
// doesn't exist yet," the common case) are treated as "nothing to rotate."
func rotateIfLarge(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxSizeBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}

// SlogBridge adapts slog.Default() to the classic Print/Printf logger
// shape several third-party libraries expect -- most notably
// github.com/creativeprojects/go-selfupdate's Logger interface, which
// nothing in this repo sets today (internal/selfupdate's own doc comment
// notes every one of its asset-search/decompress trace messages is
// currently discarded by go-selfupdate's default no-op logger). Defined
// structurally rather than by importing that package, so agentlog doesn't
// pull in go-selfupdate's dependency tree just to log through it.
type SlogBridge struct {
	// Level is the slog level every bridged message is logged at.
	// Defaults to slog.LevelInfo (its zero value) when unset.
	Level slog.Level
}

// Print implements the classic Logger interface's Print method.
func (b SlogBridge) Print(v ...any) {
	slog.Default().Log(context.Background(), b.Level, fmt.Sprint(v...))
}

// Printf implements the classic Logger interface's Printf method.
func (b SlogBridge) Printf(format string, v ...any) {
	slog.Default().Log(context.Background(), b.Level, fmt.Sprintf(format, v...))
}
