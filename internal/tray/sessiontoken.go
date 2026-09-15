package tray

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
)

// sessionTokenFileName names the file GenerateSessionToken writes beside
// agent.log. File permissions (0600) are the auth boundary for this
// single-user loopback agent -- the same reasoning the branchDAM server
// API key already relies on -- and keeping it out of argv/env avoids the
// usual process-listing exposure of a token passed as a flag.
const sessionTokenFileName = "session.token"

// GenerateSessionToken creates a fresh random token for this tray
// process's lifetime, writes it to sessionTokenFileName beside agent.log,
// and returns it so the caller can wire it into StatusServer.Token. Meant
// to be called once per tray start: a restart always gets a new token, so
// a stale token file surviving a killed process can never be replayed
// against a successor, and a crash between writing the file and the
// server actually serving never leaves a live token behind for a process
// that no longer exists.
func GenerateSessionToken() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	if err := writeSessionTokenFile(token); err != nil {
		return "", err
	}
	return token, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tray: generate session token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func writeSessionTokenFile(token string) error {
	logPath, err := agentlog.Path()
	if err != nil {
		return fmt.Errorf("tray: locate session token directory: %w", err)
	}
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tray: create session token directory: %w", err)
	}
	path := filepath.Join(dir, sessionTokenFileName)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("tray: write session token: %w", err)
	}
	return nil
}
