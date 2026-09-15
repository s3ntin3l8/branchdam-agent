//go:build windows || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

// httpTimeout bounds every request this app makes to the loopback agent.
// GET /api/status is a cheap, bounded read (Runner.Status's own
// statusQueueReadTimeout already caps the slowest part of it at 5s on the
// agent side), so this just needs to be comfortably above that, not a
// generous outer bound for something long-running like ingest.
const httpTimeout = 10 * time.Second

// App is the Wails-bound backend for the status view: one method,
// StatusJSON, kept deliberately thin. It does not decode the agent's
// response into a Go struct -- the shape is exactly whatever GET
// /api/status returns (internal/tray's statusPageView, JSON-encoded), so
// the frontend can add fields to render without a matching change here,
// and this file never drifts out of sync with what the agent actually
// sends.
type App struct {
	ctx    context.Context
	client *http.Client
}

func NewApp() *App {
	return &App{client: &http.Client{Timeout: httpTimeout}}
}

// Startup is passed to options.App.OnStartup -- Wails calls it once the
// native window/webview exists, handing back the context every other
// bound method should use so a window close cancels any in-flight request
// rather than leaking it.
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
}

// StatusJSON fetches the agent's current status. Both the server address
// (from config.yaml) and the session token are re-read on every call
// rather than cached at Startup: the agent process this window talks to
// may not be running yet, may start after this window opens, or may have
// restarted (which rotates the token, internal/sessiontoken.Generate's own
// doc comment) since the last call -- caching either value at startup
// would silently go stale in ways only a manual window restart would fix.
// There is no notification path for any of that, so every call re-resolves
// both from scratch, the same way internal/tray's own StatusFunc is
// re-invoked fresh on every request rather than cached.
func (a *App) StatusJSON() (string, error) {
	addr, err := statusServerAddrFunc()
	if err != nil {
		return "", fmt.Errorf("could not read branchDAM's config: %w", err)
	}
	token, err := sessiontoken.Read()
	if err != nil {
		return "", errors.New("branchDAM doesn't seem to be running (no session token found) -- start the tray app first")
	}

	// a.ctx is nil until Startup fires; falling back to context.Background
	// rather than passing it straight through avoids a
	// NewRequestWithContext panic if a bound method is ever invoked before
	// then (or Startup is never reached for some reason).
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", errors.New("branchDAM doesn't seem to be running (could not reach the agent) -- start the tray app first")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading the agent's response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if !json.Valid(body) {
			return "", errors.New("the agent returned a response that wasn't valid JSON")
		}
		return string(body), nil
	case http.StatusUnauthorized:
		return "", errors.New("branchDAM's session token has changed (the agent likely restarted) -- try again")
	default:
		return "", fmt.Errorf("the agent returned %s", resp.Status)
	}
}

// statusServerAddrFunc is a package-level var -- like sigstore.go's
// fetchClient and resign.go's resignAppBundle elsewhere in this repo --
// so app_test.go can point StatusJSON at an httptest.Server instead of a
// real config.yaml.
var statusServerAddrFunc = statusServerAddr

// statusServerAddr resolves the same config.yaml cmd/branchdam-agent's own
// tray subcommand resolves with no -config flag (config.ResolvePath's own
// doc comment), so this window talks to whichever agent instance an
// operator's own config already points at rather than assuming the
// default loopback port.
func statusServerAddr() (string, error) {
	path, err := config.ResolvePath("")
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return cfg.Tray.StatusAddrOrDefault(), nil
}
