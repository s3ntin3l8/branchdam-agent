// Package runtime persists the agent's per-session runtime state to a
// platform-appropriate state file. Distinct from internal/config: that
// package is the operator-edited config surface (server URL, API key,
// ingest roots, path mappings), and config.Patch is deliberately scoped
// to operator intent. Anything the agent itself writes at runtime lives
// here.
//
// Today, the only persisted field is the LastHandshakeAt timestamp the
// status page renders as "last handshake: <since> ago" -- a freshness
// signal that must survive a tray restart. Without persistence, a
// restart before the next successful handshake suppresses the line
// entirely, even if a successful handshake happened in the prior
// session (issue #149 / audit F-13 follow-up).
//
// Path mirrors internal/agentlog's own platform table for the log file
// so the two live in the same per-user state directory, separated by
// file: agent.log is the diagnostic surface, runtime.json is the
// agent-written state surface.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// State is the JSON-serializable agent runtime state. Add new fields
// here as the agent grows state worth persisting; keep the field set
// minimal -- every field is a disk format commitment and a stale
// forward-compat hazard.
type State struct {
	// LastHandshakeAt is the timestamp the agent last completed a drain
	// pass with a successful server handshake. Zero is the "never"
	// sentinel: a never-set State marshals to `{}` on disk (via the
	// custom MarshalJSON below -- time.Time's own IsZero is not
	// consulted by encoding/json's omitempty, so a bare struct field
	// would round-trip a non-empty JSON object), and a missing/empty/
	// corrupt runtime file un-marshals to the same zero State
	// (mirroring agentlog's own "the user has a working tray even if
	// the log file is unwritable" non-fatal policy: a bad runtime file
	// must not block the tray from starting).
	LastHandshakeAt time.Time `json:"lastHandshakeAt,omitempty"`
}

// MarshalJSON drops the LastHandshakeAt field entirely when it is the
// zero time.Time, so a never-set State round-trips as `{}` on disk.
// encoding/json's `omitempty` only fires when the value is its type's
// zero VALUE (false, 0, "", nil), and time.Time's zero value is a
// struct, not a string, so the default tag-driven omission is a no-op
// for a struct field. This is the same problem internal/config's own
// time-tagged patch fields have dodged by not using omitempty; the
// runtime state file is JSON-on-disk, not a YAML config, so a custom
// MarshalJSON is the right knob (and the smallest one).
func (s State) MarshalJSON() ([]byte, error) {
	type stateAlias State // avoid infinite recursion on the custom MarshalJSON
	if s.LastHandshakeAt.IsZero() {
		return []byte("{}"), nil
	}
	return json.Marshal(stateAlias(s))
}

// Path returns the platform-appropriate runtime state file location:
//   - Windows: %LOCALAPPDATA%\branchDAM\runtime.json
//   - macOS:   ~/Library/Application Support/branchDAM/runtime.json
//   - other:   $XDG_STATE_HOME/branchdam-agent/runtime.json, or
//     ~/.local/state/branchdam-agent/runtime.json when XDG_STATE_HOME
//     is unset
//
// On macOS the file lives under Application Support, not Library/Logs:
// the former is the macOS-conventional home for app-managed state, the
// latter for diagnostic logs (where internal/agentlog's agent.log
// already lives). Keeping the two in separate directories makes their
// distinct lifecycles visible at a glance -- operators inspecting
// ~/Library/Logs/branchDAM/ for logs do not find a state file they
// might mistake for diagnostic data, and vice versa.
func Path() (string, error) {
	return pathForGOOS(runtime.GOOS)
}

// pathForGOOS is Path's logic parameterized on GOOS so all three
// branches are unit-testable from a single host (matching
// internal/agentlog's own lookPathFunc-style indirection pattern)
// rather than only ever exercising whichever branch the test runner's
// own OS happens to hit.
func pathForGOOS(goos string) (string, error) {
	switch goos {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("runtime: %%LOCALAPPDATA%% is not set")
		}
		return filepath.Join(base, "branchDAM", "runtime.json"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("runtime: resolve home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "branchDAM", "runtime.json"), nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly", "solaris", "illumos", "aix":
		// XDG Base Directory Specification; the same single-
		// branch treatment internal/agentlog uses (its
		// pathForGOOS lumps all non-windows/darwin under the
		// default branch, which is equivalent for runtime
		// purposes -- `XDG_STATE_HOME` is the same env var on
		// all these targets, and `os.UserHomeDir` returns the
		// same shape). Enumerated explicitly here so adding a
		// future GOOS that needs a different path convention
		// (e.g. plan9) is a visible code change rather than
		// silently getting a Linux-style path.
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "branchdam-agent", "runtime.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("runtime: resolve home directory: %w", err)
		}
		return filepath.Join(home, ".local", "state", "branchdam-agent", "runtime.json"), nil
	default:
		return "", fmt.Errorf("runtime: unsupported GOOS %q", goos)
	}
}

// Load reads the runtime state file at path. A missing or empty file
// returns a zero State{} and a nil error -- a fresh install has no
// runtime state to load, and that's the honest signal "we have never
// completed a successful handshake", which the status page template
// already handles by suppressing the "last handshake" line.
//
// A corrupt JSON file also returns a zero State{} and a nil error:
// the file's contents are agent-written and reparsable by this same
// package, so a parse failure means the file is on its way to or
// from corruption, not a contract change that needs an error to
// surface. The parse failure is logged at WARN level so operators
// looking at agent.log can see it, and the next successful drain pass
// overwrites the file with a clean state.
//
// A permission or I/O error other than "missing" (ENOTDIR, EACCES on
// a path the agent should own) IS returned to the caller, so the
// tray can warn the user that the runtime state is not loadable
// rather than silently fall through to a zero state. The caller
// decides whether to treat the error as fatal (the tray does not).
func Load(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("runtime: read state file: %w", err)
	}
	if len(b) == 0 {
		return State{}, nil
	}
	var st State
	if jerr := json.Unmarshal(b, &st); jerr != nil {
		slog.Warn("runtime state file is corrupt; ignoring it (next successful drain will rewrite it)", "path", path, "err", jerr)
		return State{}, nil
	}
	return st, nil
}

// Save writes st to path atomically (temp file + os.Rename) at mode
// 0o600. The atomic write means a reader (this same package's Load
// during the next tray startup, or a human inspecting the file) can
// never observe a half-written state -- a torn write would parse as
// JSON, but the LastHandshakeAt it contained could be a stale timestamp
// from a concurrent save, and a status page that ever renders "successful
// 7h ago" for a write that finished 30s ago is a confusing UX.
//
// The 0o600 mode matches internal/agentlog's own tightened file mode
// for the same reason: the runtime state file is per-user agent state,
// not data the operator hands to other users, and a world-readable
// per-user JSON file in a shared home directory leaks the agent's
// drain cadence at minimum.
//
// Returns any error verbatim; the caller (Runner.onSuccessfulHandshake
// in runTrayCmd) logs and continues. Failing a Save must never block a
// drain pass -- the freshness signal is non-critical, and a
// blocking write on a full disk or a permission revocation would turn
// a 5s drain pass into a multi-second stall.
// saveOps is the testable surface of Save -- a struct of I/O
// operations that defaults to the real os package functions
// in production. The injection point lets a unit test
// exercise Save's error-path branches (CreateTemp failure,
// Write failure, Close failure, Rename failure) without
// requiring filesystem-level fault injection that's not
// reliably reproducible in a portable test. The shape mirrors
// runtimeStateOps in cmd/branchdam-agent/tray.go: a
// production-side one-liner (Save) calls saveWithOps with
// the real ops; the test side calls saveWithOps directly
// with synthetic ops that return the error under test.
type saveOps struct {
	MkdirAll   func(path string, perm os.FileMode) error
	Chmod      func(path string, perm os.FileMode) error
	CreateTemp func(dir, pattern string) (writeCloseRenamer, error)
	Rename     func(oldpath, newpath string) error
	fsyncDir   func(dir string) error
}

// writeCloseRenamer is the subset of *os.File Save uses:
// Write, Close, Name(). Bundling it into an interface lets
// the test ops return a fake that fails on Write or Close
// without spinning up a real temp file.
type writeCloseRenamer interface {
	Write(p []byte) (n int, err error)
	Close() error
	Name() string
}

// Save persists st to path atomically (temp file + os.Rename) at mode
// 0o600. The atomic write means a reader (this same package's Load
// during the next tray startup, or a human inspecting the file) can
// never observe a half-written state -- a torn write would parse as
// JSON, but the LastHandshakeAt it contained could be a stale timestamp
// from a concurrent save, and a status page that ever renders "successful
// 7h ago" for a write that finished 30s ago is a confusing UX.
//
// The 0o600 mode matches internal/agentlog's own tightened file mode
// for the same reason: the runtime state file is per-user agent state,
// not data the operator hands to other users, and a world-readable
// per-user JSON file in a shared home directory leaks the agent's
// drain cadence at minimum.
//
// Returns any error verbatim; the caller (Runner.onSuccessfulHandshake
// in runTrayCmd) logs and continues. Failing a Save must never block a
// drain pass -- the freshness signal is non-critical, and a
// blocking write on a full disk or a permission revocation would turn
// a 5s drain pass into a multi-second stall.
func Save(path string, st State) error {
	return saveWithOps(path, st, saveOps{
		MkdirAll:   os.MkdirAll,
		Chmod:      os.Chmod,
		CreateTemp: func(dir, pattern string) (writeCloseRenamer, error) { return os.CreateTemp(dir, pattern) },
		Rename:     os.Rename,
		fsyncDir:   fsyncDir,
	})
}

func saveWithOps(path string, st State, ops saveOps) error {
	// Create the parent directory at 0o700, not the more common
	// 0o755: a world-readable per-user state dir lets any local
	// user `os.Stat` the directory and infer the existence of
	// branchdam-agent on this host, and `os.ReadDir` to list the
	// runtime.json file's mtime (a leakage of the agent's drain
	// cadence). MkdirAll's mode argument is only used when the
	// directory has to be created; for an existing directory
	// (e.g. one created by a pre-issue-#149 build of agentlog
	// that lived alongside this code at 0o755) the chmod below
	// tightens the mode to 0o700 on every save. A chmod failure
	// is best-effort and logged at WARN -- a non-POSIX filesystem
	// that doesn't support chmod (Windows ACLs, FAT32 camera cards
	// mounted as USB storage) must not block the save.
	if err := ops.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("runtime: create state directory: %w", err)
	}
	if err := ops.Chmod(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("runtime: could not tighten state directory mode to 0700 (non-fatal)", "path", filepath.Dir(path), "err", err)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("runtime: marshal state: %w", err)
	}
	tmp, err := ops.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("runtime: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		// Best-effort: if the rename succeeded, the temp is already
		// gone; if it didn't, the temp is a stale file that Load will
		// not see (it only reads `path`, not `path+".tmp.*"`) but that
		// still takes up disk space. Clean it up regardless.
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("runtime: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("runtime: close temp file: %w", err)
	}
	if err := ops.Chmod(tmpPath, 0o600); err != nil {
		// Best-effort: on Windows ACLs and FAT32-mounted media chmod
		// is a no-op or returns an error. A 0o600 failure on the
		// *temp* file is non-fatal -- the rename will preserve the
		// pre-tightening mode of the destination if it already
		// existed. Log so the next restart's agentlog can show it.
		slog.Warn("runtime: could not tighten temp file mode to 0600 (non-fatal)", "path", tmpPath, "err", err)
	}
	if err := ops.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("runtime: rename temp file to %q: %w", path, err)
	}
	// Fsync the parent directory after the rename so the directory
	// entry pointing at the new inode reaches disk before the next
	// power loss. Without this, a crash between os.Rename returning
	// and the directory's inode reaching the journal can leave the
	// directory entry pointing at the *old* inode on next boot --
	// the temp+rename atomicity is moot if the dir entry never
	// commits. Same risk class as the in-PR carry-forward
	// invariant: the freshness signal the file preserves is
	// non-critical, so an `errno == ENOTSUP` (some non-POSIX
	// filesystems return ENOTSUP on dir fsync) is best-effort and
	// non-fatal -- we log at WARN and continue. The in-memory
	// carry-forward is the primary correctness layer; this fsync
	// only defends the cross-session half.
	if err := ops.fsyncDir(filepath.Dir(path)); err != nil {
		slog.Warn("runtime: could not fsync parent directory (non-fatal; a power loss could revert to the prior runtime.json)", "path", filepath.Dir(path), "err", err)
	}
	return nil
}
