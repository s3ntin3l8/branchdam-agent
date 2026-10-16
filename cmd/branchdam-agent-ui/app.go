//go:build windows || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

// httpTimeout bounds every request this app makes to the loopback agent.
// GET /api/status is a cheap, bounded read (Runner.Status's own
// statusQueueReadTimeout already caps the slowest part of it at 5s on the
// agent side), so this just needs to be comfortably above that, not a
// generous outer bound for something long-running like ingest.
const httpTimeout = 10 * time.Second

// errAgentBusy is agentRequest's error when the agent accepted the
// connection but did not answer within httpTimeout.
var errAgentBusy = errors.New("branchDAM is running but didn't answer in time -- it may be busy with an ingest or update; a settings change may already be saved, so reopen Settings to check")

// errAgentNoAnswer is agentRequest's error when httpTimeout ran out before
// a connection to the agent was even established (e.g. a firewall
// silently dropping packets to the loopback port). Nothing proves the
// agent is running then, so the wording claims neither way.
var errAgentNoAnswer = errors.New("branchDAM didn't answer in time (no connection to the agent was made) -- check that the tray app is running and that nothing blocks its local port")

// passHTTPTimeout bounds the window's "Drain now"/"Prune now" requests,
// which (unlike every other call here) wait for a whole pass to finish.
// The agent bounds the pass itself at 2 minutes (internal/tray's
// drainPruneClickTimeout); this sits comfortably above it so the window
// reports the pass's own result rather than its own timeout.
const passHTTPTimeout = 3 * time.Minute

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
	// passClient is client's counterpart for the two calls that wait on a
	// whole drain or prune pass (passHTTPTimeout).
	passClient *http.Client
}

func NewApp() *App {
	return &App{
		client:     &http.Client{Timeout: httpTimeout},
		passClient: &http.Client{Timeout: passHTTPTimeout},
	}
}

// Startup is passed to options.App.OnStartup -- Wails calls it once the
// native window/webview exists, handing back the context every other
// bound method should use so a window close cancels any in-flight request
// rather than leaking it.
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
}

// Version returns this UI binary's own stamped version (main.version, "dev"
// for a local unstamped build) -- distinct from StatusJSON's "version"
// field, which is the AGENT's own version. The two ship together from the
// same release and should normally match, but can briefly disagree if a
// self-update fails partway (see main.go's version var doc comment).
func (a *App) Version() string {
	return version
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

// PathMappingInput mirrors internal/tray's PathMappingEntry -- duplicated
// rather than imported, same reasoning as idActionRequest below: this
// package talks to the agent purely over the loopback HTTP API, never
// importing internal/tray's Go types directly.
type PathMappingInput struct {
	WorkstationPath string `json:"workstationPath"`
	ContainerPath   string `json:"containerPath"`
}

// SetPathMappings replaces the whole pathMappings list -- POST
// /api/settings/path-mappings's counterpart, and the only way to set path
// mappings: the comma/colon string route SetSetting("pathMappings", ...)
// used to accept was retired (issue #236) because it was lossy for a path
// containing a comma. A separate method from SetSetting for the same
// reason SetIntegrationRewrites is one: mappings parse into a structured
// value the generic key/value route never handles. A nil or empty
// mappings clears the list.
func (a *App) SetPathMappings(mappings []PathMappingInput) (string, error) {
	reqBody, err := json.Marshal(struct {
		Mappings []PathMappingInput `json:"mappings"`
	}{Mappings: mappings})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/settings/path-mappings", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TriggerSync runs one catalog-sync pass for integration id right now --
// POST /api/actions/sync's counterpart, mirroring internal/tray/
// integrationsmenu.go's own "Sync now" menu item. Returns the raw
// syncActionResult JSON (internal/tray/statusapi.go) so the frontend can
// render pairsFound/emitted/skipped/err without a matching struct here --
// same "never drifts out of sync" reasoning as StatusJSON's own doc
// comment. This call runs synchronously to completion (TriggerSync's own
// doc comment): a real sync pass can take seconds, so the frontend should
// disable its "Sync now" button for the duration rather than assume this
// returns quickly.
func (a *App) TriggerSync(id string) (string, error) {
	reqBody, err := json.Marshal(idActionRequest{ID: id})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/sync", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TestConnection runs one on-demand server-reachability check right now --
// POST /api/actions/test-connection's counterpart, mirroring TriggerSync's
// own shape above: no request body, and the raw testConnectionActionResult
// JSON (internal/tray/statusapi.go) returned as-is so the frontend can
// render ok/version/err without a matching struct duplicated here. Unlike
// TriggerDrain, this check is meaningful with no offline queue configured
// at all -- see tray.ServerProbe's own doc comment -- which is the whole
// reason the Server card needs its own button rather than reusing a
// "Drain queue now" affordance the window never exposed in the first
// place.
func (a *App) TestConnection() (string, error) {
	body, err := a.agentRequest(http.MethodPost, "/api/actions/test-connection", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Pair submits a branchdam:// pairing URL to the agent's loopback API --
// POST /api/actions/pair's counterpart. Returns the raw pairActionResult
// JSON (internal/tray/statusapi.go) so the frontend can check ok and err.
func (a *App) Pair(rawURL string) (string, error) {
	reqBody, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: rawURL})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/pair", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// PairDeepLink forwards a branchdam:// URL the OS handed to this process
// (macOS delivers it to whichever process has focus -- this one while its
// window is open) to the tray, marked as a deep link so the tray asks the
// operator to confirm before writing any config. Returns immediately with
// the tray's 202; the confirmation and outcome happen in the tray.
func (a *App) PairDeepLink(rawURL string) (string, error) {
	reqBody, err := json.Marshal(struct {
		URL    string `json:"url"`
		Source string `json:"source"`
	}{URL: rawURL, Source: "deeplink"})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/pair", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// HandleOpenURL is the Wails OnUrlOpen callback: forward the link to the tray
// and, if that fails (tray down or restarting), tell the operator -- a log line
// alone would reproduce the original click-does-nothing. The message comes from
// agentRequest's fixed wording and never contains the URL (it embeds the key).
func (a *App) HandleOpenURL(rawURL string) {
	_, err := a.PairDeepLink(rawURL)
	if err == nil {
		return
	}
	slog.Warn("could not forward branchdam:// link to the tray", "err", agentlog.Sanitize(err.Error()))
	if a.ctx == nil {
		return
	}
	_, _ = messageDialogFunc(a.ctx, runtime.MessageDialogOptions{
		Type:    runtime.ErrorDialog,
		Title:   "branchDAM",
		Message: "Couldn't start pairing: " + err.Error(),
	})
}

// CheckForUpdate runs one on-demand self-update check right now --
// POST /api/actions/check-update's counterpart, the same no-request-body
// shape as TestConnection above. Read-only (never touches the installed
// binary), so unlike an eventual Apply/Rollback exposure this needs no
// idle-lock reasoning on the agent side. Returns the raw
// checkUpdateActionResult JSON (internal/tray/statusapi.go) as-is, same
// "never drifts out of sync" reasoning as every other action method here.
func (a *App) CheckForUpdate() (string, error) {
	body, err := a.agentRequest(http.MethodPost, "/api/actions/check-update", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ApplyUpdate starts an approved self-update through the tray's authenticated
// loopback API. The action returns immediately; the frontend continues to
// poll status so the window can show downloading/verification/error phases
// without blocking its event loop for the duration of the archive transfer.
func (a *App) ApplyUpdate() (string, error) {
	body, err := a.agentRequest(http.MethodPost, "/api/actions/apply-update", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ConfirmApplyUpdate renders a native question dialog before the destructive
// update action. Browser confirm() is not reliable in Wails' macOS WKWebView,
// so this stays in the Go binding and uses the platform-native dialog API.
// Note: on Windows, Wails maps QuestionDialog to Win32 MessageBox(MB_YESNO),
// which ignores custom button labels and returns "Yes" / "No". On macOS, Wails
// returns the custom button label ("Install and restart"). Both are accepted
// (along with "Ok" as defense-in-depth if dialog types ever change).
func (a *App) ConfirmApplyUpdate(latestVersion string) (bool, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	selected, err := messageDialogFunc(ctx, runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         "Confirm install and restart",
		Message:       fmt.Sprintf("Install update %s and restart the tray? The tray will be unavailable for ~5 seconds.", latestVersion),
		Buttons:       []string{"Cancel", "Install and restart"},
		DefaultButton: "Install and restart",
		CancelButton:  "Cancel",
	})
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(selected)) {
	case "install and restart", "yes", "ok": // "ok" is defensive in case dialog options or bindings change
		return true, nil
	default:
		return false, nil
	}
}

// TriggerHookInstall installs (or reinstalls) id's render hook right now --
// POST /api/actions/hook-install's counterpart, mirroring
// internal/tray/hooksmenu.go's own "Install / update render hook" item.
// Returns the raw hookInstallActionResult JSON, same reasoning as
// TriggerSync above.
func (a *App) TriggerHookInstall(id string) (string, error) {
	reqBody, err := json.Marshal(idActionRequest{ID: id})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/hook-install", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// RevealHook opens id's Scripts folder in the OS file manager --
// POST /api/actions/hook-reveal's counterpart, mirroring
// internal/tray/hooksmenu.go's own "Reveal Scripts folder" item. Unlike
// TriggerSync/TriggerHookInstall, the response body carries nothing worth
// returning to the frontend on success (Runner.RevealHook's own doc
// comment: "fire-and-forget", no state mutation) -- only its err field is
// worth surfacing, so this returns a plain error rather than a JSON string
// for the frontend to re-parse.
func (a *App) RevealHook(id string) error {
	reqBody, err := json.Marshal(idActionRequest{ID: id})
	if err != nil {
		return err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/hook-reveal", reqBody)
	if err != nil {
		return err
	}
	var res struct {
		Err string `json:"err"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return err
	}
	if res.Err != "" {
		return errors.New(res.Err)
	}
	return nil
}

// StartIngest starts an ingest in the agent and returns at once --
// POST /api/actions/ingest-start's counterpart. An empty cardPath is the
// tray menu's "Ingest now" (every detected card volume); a folder path is
// "Import from folder…" (pick it with PickDirectory first). The agent runs
// the ingest through the same Runner.TriggerIngest the tray menu uses, so
// it is serialized with every other ingest and self-update; the window
// follows it through StatusJSON's busy/ingestProgress fields. Returns the
// raw ingestStartResult JSON: started=false with err says why nothing
// started (paused, already running, no card found).
func (a *App) StartIngest(cardPath string) (string, error) {
	reqBody, err := json.Marshal(struct {
		CardPath string `json:"cardPath"`
	}{CardPath: cardPath})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/ingest-start", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// CancelIngest asks the in-flight ingest to stop after the file it is
// copying -- POST /api/actions/cancel-ingest's counterpart. The file in
// progress always finishes (copy, verify and registration), so nothing is
// left half-written; the card is not ejected and re-ingesting it later
// picks up where this stopped. Returns the raw {"canceled": bool} JSON;
// false means no ingest was running.
func (a *App) CancelIngest() (string, error) {
	body, err := a.agentRequest(http.MethodPost, "/api/actions/cancel-ingest", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TriggerDrain runs one offline-queue drain pass now and waits for it --
// POST /api/actions/drain's counterpart, the tray menu's "Drain queue
// now". Returns the raw drainActionResult JSON; ran=false means the pass
// was skipped (paused, metered connection, already running, or not
// configured).
func (a *App) TriggerDrain() (string, error) {
	body, err := a.agentRequestWith(a.passClient, http.MethodPost, "/api/actions/drain", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TriggerPrune runs one prune pass now and waits for it --
// POST /api/actions/prune's counterpart, the tray menu's "Prune now".
// Returns the raw pruneActionResult JSON; ran=false means skipped
// (paused, an ingest or update holds the gate, or prune is off).
func (a *App) TriggerPrune() (string, error) {
	body, err := a.agentRequestWith(a.passClient, http.MethodPost, "/api/actions/prune", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// SetPaused turns the agent's session-only pause on or off --
// POST /api/actions/pause's counterpart, the tray menu's "Pause ingest" /
// "Resume ingest". Never the persisted pauseUploadOnMetered setting (that
// is SetSetting("ingest.pauseUploadOnMetered", ...)). Returns the raw
// {"paused": bool} JSON with the state now in effect.
func (a *App) SetPaused(paused bool) (string, error) {
	reqBody, err := json.Marshal(struct {
		Paused bool `json:"paused"`
	}{Paused: paused})
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/pause", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// RollbackUpdate starts restoring the previously installed version --
// POST /api/actions/rollback-update's counterpart, the tray menu's "Roll
// back to …". Returns immediately with the raw {"started", "status"} JSON;
// the swap and the tray's restart are followed through StatusJSON's
// selfUpdate phase, like ApplyUpdate.
func (a *App) RollbackUpdate() (string, error) {
	body, err := a.agentRequest(http.MethodPost, "/api/actions/rollback-update", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FailedQueueJSON lists the offline queue's FAILED rows --
// GET /api/queue/failed's counterpart. Returns the raw
// {"configured", "rows"} JSON.
func (a *App) FailedQueueJSON() (string, error) {
	body, err := a.agentRequest(http.MethodGet, "/api/queue/failed", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// RetryFailedRow puts one FAILED queue row back to pending so the next
// drain pass retries it -- POST /api/actions/queue-retry with nodeUuid.
// Returns the raw {"archiveCopies", "rebases"} reset counts.
func (a *App) RetryFailedRow(nodeUUID string) (string, error) {
	if nodeUUID == "" {
		return "", errors.New("no queue row given to retry")
	}
	return a.queueRetry(struct {
		NodeUUID string `json:"nodeUuid"`
	}{NodeUUID: nodeUUID})
}

// RetryAllFailed puts every FAILED queue row back to pending -- the same
// reset `queue-drain -retry-failed` runs.
func (a *App) RetryAllFailed() (string, error) {
	return a.queueRetry(struct {
		All bool `json:"all"`
	}{All: true})
}

func (a *App) queueRetry(req any) (string, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	body, err := a.agentRequest(http.MethodPost, "/api/actions/queue-retry", reqBody)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ConfirmAction asks the operator to confirm a destructive window action
// (drain, prune, roll back, cancel ingest) with a native question dialog
// -- ConfirmApplyUpdate's generalization, for the same WKWebView reason
// browser confirm() can't be used. The frontend only calls it when the
// tray.confirmDestructive setting is on, mirroring the tray menu's own
// gate. Windows' MessageBox ignores custom labels and returns "Yes"/"No";
// macOS returns the label itself -- both are accepted.
func (a *App) ConfirmAction(title, message, confirmLabel string) (bool, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if confirmLabel == "" {
		confirmLabel = "OK"
	}
	selected, err := messageDialogFunc(ctx, runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         title,
		Message:       message,
		Buttons:       []string{"Cancel", confirmLabel},
		DefaultButton: confirmLabel,
		CancelButton:  "Cancel",
	})
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(selected)) {
	case strings.ToLower(confirmLabel), "yes", "ok":
		return true, nil
	default:
		return false, nil
	}
}

// idActionRequest mirrors internal/tray/statusapi.go's own request shape of
// the same name -- {"id": "..."} -- for /api/actions/sync,
// /api/actions/hook-install, and /api/actions/hook-reveal. Duplicated here
// rather than imported: this package talks to the agent purely over the
// loopback HTTP API (main.go's own package doc comment), never importing
// internal/tray's Go types directly.
type idActionRequest struct {
	ID string `json:"id"`
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
	return a.agentRequestWith(a.client, method, path, body)
}

// agentRequestWith is agentRequest over a specific client -- passClient
// for the drain/prune calls that wait on a whole pass, client otherwise.
func (a *App) agentRequestWith(client *http.Client, method, path string, body []byte) ([]byte, error) {
	if client == nil {
		client = a.client
	}
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
	// connected records whether the transport got a connection to the
	// agent, so a timeout can tell "running but slow" from "never
	// reached". Atomic: the trace hook may run on the transport's dial
	// goroutine.
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		// A timeout on an established connection means the agent
		// accepted the request but didn't answer in time: it is running,
		// just busy. Saying "not running" there sent operators restarting
		// a tray that had in fact saved their change. A timeout before
		// any connection (a dial that never completed) proves nothing
		// either way, so it gets neutral wording.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if connected.Load() {
				return nil, errAgentBusy
			}
			return nil, errAgentNoAnswer
		}
		return nil, errors.New("branchDAM doesn't seem to be running (could not reach the agent) -- start the tray app first")
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading the agent's response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted:
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

// messageDialogFunc is a seam for the native confirmation dialog. Keeping it
// indirect makes the button/selection contract testable on a cross-compiled
// Darwin/Windows package without opening a real desktop dialog in CI.
var messageDialogFunc = runtime.MessageDialog

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
