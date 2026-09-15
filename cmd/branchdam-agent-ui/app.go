//go:build windows || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

// httpTimeout bounds every request this app makes to the loopback agent.
// GET /api/status is a cheap, bounded read (Runner.Status's own
// statusQueueReadTimeout already caps the slowest part of it at 5s on the
// agent side), so this just needs to be comfortably above that, not a
// generous outer bound for something long-running like ingest.
const httpTimeout = 10 * time.Second

// App is the Wails-bound backend for the status and settings views, kept
// deliberately thin: every settings/status method below round-trips a raw
// JSON string to and from the agent's loopback API rather than decoding
// into a Go struct -- the shape is exactly whatever the agent returns
// (internal/tray's statusPageView / SettingsView, JSON-encoded), so the
// frontend can add fields to render without a matching change here, and
// this file never drifts out of sync with what the agent actually sends.
// The agent's session token is read fresh on every call and never handed
// to the frontend (see agentRequest) -- keeping it out of the webview's JS
// context entirely is why settings mutations are bound Go methods rather
// than plain fetch() calls from app.js.
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
	body, err := a.agentRequest(http.MethodGet, "/api/status", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SettingsJSON fetches the agent's current settings snapshot -- GET
// /api/settings's counterpart to StatusJSON, same re-resolve-everything-
// per-call reasoning (StatusJSON's own doc comment).
func (a *App) SettingsJSON() (string, error) {
	body, err := a.agentRequest(http.MethodGet, "/api/settings", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SetSetting persists one settings key -- POST /api/settings's
// counterpart. value can be any JSON-serializable type that route
// accepts (bool, number, string, or array of strings); see
// internal/tray/statusapi.go's settingsPatchRequest doc comment for which
// Settings setter each type routes to. Returns the updated settings
// snapshot on success, the same shape SettingsJSON returns, so the
// frontend can re-render from the response without a second round-trip.
func (a *App) SetSetting(key string, value any) (string, error) {
	reqBody, err := json.Marshal(struct {
		Key   string `json:"key"`
		Value any    `json:"value"`
	}{Key: key, Value: value})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/settings", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SetIntegrationPath persists a per-integration catalog path or database
// URL -- POST /api/settings/integration-path's counterpart. A separate
// route (and method) from SetSetting because the actual config key
// (catalogPath vs. databaseUrl for Resolve) is resolved server-side from
// id, not something this frontend can spell out itself.
func (a *App) SetIntegrationPath(id, value string) (string, error) {
	reqBody, err := json.Marshal(struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}{ID: id, Value: value})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/settings/integration-path", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SetIntegrationRewrites persists an integration's path rewrite rules --
// POST /api/settings/integration-rewrites's counterpart. Not reachable
// through SetSetting: rewrites parse into a structured value the generic
// key/value route never handles (internal/tray/statusapi.go's
// idValueSettingsRequest doc comment).
func (a *App) SetIntegrationRewrites(id, value string) (string, error) {
	reqBody, err := json.Marshal(struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}{ID: id, Value: value})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/settings/integration-rewrites", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// PickDirectory opens a native folder picker and returns the chosen path,
// or "" if the operator canceled. Wails' OpenDirectoryDialog is only
// reachable from Go code running inside this process's own window context
// (it needs the native window handle) -- app.js cannot call it directly,
// which is why every folder-picker field routes through this bound
// method instead of a plain HTML <input type="file" webkitdirectory>.
func (a *App) PickDirectory(title string) (string, error) {
	ctx := a.ctx
	if ctx == nil {
		return "", errors.New("window is not ready yet")
	}
	return runtime.OpenDirectoryDialog(ctx, runtime.OpenDialogOptions{Title: title})
}

// PickFile is PickDirectory's counterpart for a single file, with an
// optional filename filter (e.g. []string{"*.json"} for the node-index
// picker). An empty patterns shows every file.
func (a *App) PickFile(title string, patterns []string) (string, error) {
	ctx := a.ctx
	if ctx == nil {
		return "", errors.New("window is not ready yet")
	}
	opts := runtime.OpenDialogOptions{Title: title}
	if len(patterns) > 0 {
		opts.Filters = []runtime.FileFilter{{
			DisplayName: strings.Join(patterns, ", "),
			Pattern:     strings.Join(patterns, ";"),
		}}
	}
	return runtime.OpenFileDialog(ctx, opts)
}

// agentRequest issues method to the agent's loopback API at path (e.g.
// "/api/settings"), with body as the JSON request body (nil for none),
// and returns the raw response body. StatusJSON and every settings method
// above share this one place for address/token resolution and the "agent
// not running"/"token changed" error wording, so every bound method fails
// the same way for the same underlying reason.
func (a *App) agentRequest(method, path string, body []byte) ([]byte, error) {
	addr, err := statusServerAddrFunc()
	if err != nil {
		return nil, fmt.Errorf("could not read branchDAM's config: %w", err)
	}
	token, err := sessiontoken.Read()
	if err != nil {
		return nil, errors.New("branchDAM doesn't seem to be running (no session token found) -- start the tray app first")
	}

	// a.ctx is nil until Startup fires; falling back to context.Background
	// rather than passing it straight through avoids a
	// NewRequestWithContext panic if a bound method is ever invoked before
	// then (or Startup is never reached for some reason).
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errors.New("branchDAM doesn't seem to be running (could not reach the agent) -- start the tray app first")
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the agent's response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if !json.Valid(respBody) {
			return nil, errors.New("the agent returned a response that wasn't valid JSON")
		}
		return respBody, nil
	case http.StatusUnauthorized:
		return nil, errors.New("branchDAM's session token has changed (the agent likely restarted) -- try again")
	default:
		// The agent's error handlers (internal/tray/statusapi.go) write
		// the failure reason as a plain-text body via http.Error -- surface
		// it instead of just the status line, since it's usually the only
		// clue why a settings change was rejected (e.g. a validation
		// error from Settings.SetString).
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		return nil, fmt.Errorf("the agent returned %s: %s", resp.Status, msg)
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
