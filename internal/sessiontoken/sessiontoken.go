// Package sessiontoken manages the shared secret internal/tray's loopback
// StatusServer (see its own statusapi.go) requires on every /api/* request,
// and that cmd/branchdam-agent-ui reads to authenticate against that same
// server. Split out of internal/tray specifically so the UI binary never
// needs to import internal/tray -- that package's fyne.io/systray
// dependency has no business in a process with no tray menu of its own.
package sessiontoken

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
)

// FileName names the file Generate writes, and Read reads, beside
// agent.log. File permissions (0600) are the auth boundary for this
// single-user loopback agent -- the same reasoning the branchDAM server
// API key already relies on -- and keeping it out of argv/env avoids the
// usual process-listing exposure of a token passed as a flag.
const FileName = "session.token"

// Generate creates a fresh random token for this tray process's lifetime,
// writes it to FileName beside agent.log, and returns it so the caller can
// wire it into StatusServer.Token. Meant to be called once per tray start:
// a restart always gets a new token, so a stale token file surviving a
// killed process can never be replayed against a successor, and a crash
// between writing the file and the server actually serving never leaves a
// live token behind for a process that no longer exists.
func Generate() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := write(token); err != nil {
		return "", err
	}
	return token, nil
}

// Read returns the token the running tray last wrote via Generate. Callers
// (cmd/branchdam-agent-ui) should call this fresh before every request
// rather than caching the result: the tray may not be running at all, or
// may have restarted (and rotated its token) since the caller last read
// it, and there is no notification path for either -- a stale cached
// token just gets a 401 on the next request. A missing file is reported
// via the file-not-found error as-is, deliberately, so a caller can give
// its own "the agent doesn't appear to be running" message rather than
// this package guessing at one.
func Read() (string, error) {
	path, err := path()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("sessiontoken: generate: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func path() (string, error) {
	logPath, err := agentlog.Path()
	if err != nil {
		return "", fmt.Errorf("sessiontoken: locate directory: %w", err)
	}
	return filepath.Join(filepath.Dir(logPath), FileName), nil
}

func write(token string) error {
	path, err := path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sessiontoken: create directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("sessiontoken: write: %w", err)
	}
	// os.WriteFile only applies its mode argument when it creates the
	// file -- a pre-existing session.token left behind with looser
	// permissions (a prior build's bug, a manual edit) would otherwise
	// keep them. Chmod unconditionally, after every write, to retighten
	// regardless of whether this call created or overwrote the file.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("sessiontoken: tighten permissions: %w", err)
	}
	return nil
}
