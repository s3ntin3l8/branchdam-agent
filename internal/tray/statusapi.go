package tray

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// ActionRunner is the subset of *Runner the status server's mutating
// /api/actions/* routes drive. A structural interface, like Drainer/
// Pruner/Settings elsewhere in this package, rather than a *Runner field
// directly -- so a test can inject a fake without constructing a real
// Runner's watch dirs, gates, and queue reader. *Runner already satisfies
// this with no changes: TriggerIngest/TriggerDrain/TriggerPrune/
// TriggerSync/TriggerHookInstall/RevealHook/Paused/SetPaused are all
// already safe to call concurrently from an HTTP handler goroutine (see
// each method's own doc comment for its serialization mechanism).
//
// The window's ingest controls (POST /api/actions/ingest-start and
// /api/actions/cancel-ingest) and its failed-rows list
// (GET /api/queue/failed, POST /api/actions/queue-retry) go through the
// same Runner: IngestNowVolumes/WatchDirs pick what "Ingest now" and
// "Import from folder…" ingest exactly as the tray menu does, every
// ingest still runs through TriggerIngest (and so behind Runner.gate,
// AGENTS.md invariant 9), CancelIngest stops it at a file boundary, and
// the failed-row methods wrap the queue store's own FailedRows/
// ResetFailedNode/ResetFailed. This file never reimplements any of them.
type ActionRunner interface {
	TriggerIngest(ctx context.Context, cardPath string) IngestSummary
	TriggerIngestNoWait(ctx context.Context, cardPath string) IngestSummary
	TriggerDrain(ctx context.Context) (DrainSummary, bool)
	TriggerPrune(ctx context.Context) (PruneSummary, bool)
	TriggerSync(ctx context.Context, id IntegrationID) (SyncSummary, bool)
	TriggerHookInstall(ctx context.Context, id HookID) (HookState, bool)
	TriggerServerProbe(ctx context.Context) (ProbeResult, bool)
	RevealHook(id HookID) error
	Paused() bool
	SetPaused(v bool)
	CancelIngest() bool
	Busy() (cardPath string, since time.Time, busy bool)
	GateHeld() bool
	ConfigIncomplete() bool
	IngestNowVolumes() []string
	WatchDirs() []string
	FailedQueueRows(ctx context.Context, limit int) ([]FailedQueueRow, error)
	RetryFailedQueueRows(ctx context.Context, nodeUUID string) (QueueRetryCounts, error)
}

// UpdateChecker is the one-method subset of the self-update subsystem the
// status API's on-demand check drives -- StatusServer.Updates' own doc
// comment explains why this is a narrow interface separate from
// tray.SelfUpdater (the tray menu's contract for ApplyLatest/Rollback)
// rather than a new method on that one. CheckNow runs (or reports it
// couldn't run) a self-update check right now and returns the resulting
// UpdateStatus either way -- ran=false means "nothing changed": disabled,
// a non-semver build, or a check already in flight, none of which are
// errors.
type UpdateChecker interface {
	CheckNow(ctx context.Context) (status UpdateStatus, ran bool)
}

// UpdateApplier is the asynchronous self-update action exposed to the native
// window. StartApply returns immediately after the caller has acquired the
// same idle gate used by the tray menu; the lifecycle and any eventual error
// are observed through UpdateStatus polling. Keeping this separate from
// UpdateChecker avoids making the read-only check contract responsible for
// process replacement and restart behavior.
type UpdateApplier interface {
	StartApply() (status UpdateStatus, started bool)
}

// UpdateRollbacker is UpdateApplier's counterpart for restoring the
// previous version from the window. StartRollback returns immediately
// after acquiring the same idle gate (Runner.TryLockIdle) the tray menu's
// "Roll back" holds, so a rollback can never swap binaries under an
// in-flight ingest; the swap and the restart that follows it are observed
// through UpdateStatus polling (Phase rolling-back, then restarting with
// RolledBack set).
type UpdateRollbacker interface {
	StartRollback() (status UpdateStatus, started bool)
}

// drainPruneClickTimeout bounds an on-demand drain or prune pass, whether
// a tray menu click or a window button started it -- the tray's own
// background timer path (cmd/branchdam-agent/queueagent.go's
// periodicPassTimeout) already bounds every automatic pass to the same
// duration; a manual click went unbounded until a Hermes review finding
// caught the inconsistency. The status API applies it too: its handlers
// detach from the request context (context.WithoutCancel), so without a
// deadline a wedged pass would hold drainMu (or Runner.gate, for prune)
// indefinitely.
const drainPruneClickTimeout = 2 * time.Minute

// isLoopbackHost reports whether addr's host resolves to the loopback
// interface. Used once, at route-registration time in Serve, to decide
// whether the /api/* routes get registered at all -- see Serve's own
// comment for why this is a registration-time decision rather than a
// per-request check: tray.statusAddr is hand-editable, and an operator who
// has explicitly widened the bind (matching normalizeLoopback's own
// deliberate pass-through of an explicit non-loopback host) has already
// accepted the read-only status page being reachable off the workstation.
// Adding mutating actuators on top of that without an explicit opt-in
// would be a materially bigger blast radius than today's information leak.
func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originAllowed rejects requests a browser's fetch/XHR made on behalf of a
// page loaded from somewhere else -- the CSRF/DNS-rebinding shape this
// loopback server is otherwise defenseless against, since a browser will
// happily let http://evil.example issue a same-machine POST to
// 127.0.0.1:38080 and attach cookies/no-auth by default. Two independent
// signals, either one sufficient to reject:
//
//   - Sec-Fetch-Site, when a browser sends it (Chrome/Firefox on same-site
//     or cross-site navigations and fetches) -- "cross-site" is rejected
//     outright regardless of Origin.
//   - Origin, when present, must match this server's own addr exactly.
//
// A request with neither header (curl, or a native Wails frontend's HTTP
// client, which doesn't run inside a browser's fetch sandbox) passes this
// check -- there is nothing to compare against, and the token requirement
// in withAPIAuth is what actually gates those callers.
func originAllowed(r *http.Request, addr string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return origin == "http://"+addr
}

// tokenValid reports whether r carries the "Authorization: Bearer <token>"
// header matching s.Token, compared in constant time so response timing
// can't be used to brute-force the token byte by byte. An empty s.Token
// (sessiontoken.Generate failed at tray startup -- see tray.go's call site)
// fails closed: every /api/* request is rejected rather than the check
// degrading to "any token accepted".
func (s *StatusServer) tokenValid(r *http.Request) bool {
	if s.Token == "" {
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	got := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

// withAPIAuth wraps every /api/* handler with the origin check and the
// token check, in that order -- origin first, since it's the cheaper
// check and rejects the browser-CSRF shape before even looking at the
// token.
func (s *StatusServer) withAPIAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r, s.Addr) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		if !s.tokenValid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// registerAPIRoutes adds the hardened /api/* surface to mux, called from
// Serve. Registration -- not just enforcement -- is gated on
// isLoopbackHost: an operator-widened bind address gets none of these
// routes at all (a 404, indistinguishable from a build that predates this
// feature) rather than a 403 that would still confirm the routes exist.
func (s *StatusServer) registerAPIRoutes(mux *http.ServeMux) {
	if !isLoopbackHost(s.Addr) {
		return
	}
	mux.HandleFunc("GET /api/status", s.withAPIAuth(s.handleStatusJSON))
	mux.HandleFunc("GET /api/settings", s.withAPIAuth(s.handleAPISettingsGet))
	mux.HandleFunc("POST /api/settings", s.withAPIAuth(s.handleAPISettingsPost))
	mux.HandleFunc("POST /api/settings/integration-path", s.withAPIAuth(s.handleAPISettingsIntegrationPath))
	mux.HandleFunc("POST /api/settings/integration-rewrites", s.withAPIAuth(s.handleAPISettingsIntegrationRewrites))
	mux.HandleFunc("POST /api/settings/path-mappings", s.withAPIAuth(s.handleAPISettingsPathMappings))
	mux.HandleFunc("POST /api/actions/ingest", s.withAPIAuth(s.handleActionIngest))
	mux.HandleFunc("POST /api/actions/drain", s.withAPIAuth(s.handleActionDrain))
	mux.HandleFunc("POST /api/actions/prune", s.withAPIAuth(s.handleActionPrune))
	mux.HandleFunc("POST /api/actions/sync", s.withAPIAuth(s.handleActionSync))
	mux.HandleFunc("POST /api/actions/hook-install", s.withAPIAuth(s.handleActionHookInstall))
	mux.HandleFunc("POST /api/actions/hook-reveal", s.withAPIAuth(s.handleActionHookReveal))
	mux.HandleFunc("POST /api/actions/pause", s.withAPIAuth(s.handleActionPause))
	mux.HandleFunc("POST /api/actions/test-connection", s.withAPIAuth(s.handleActionTestConnection))
	mux.HandleFunc("POST /api/actions/check-update", s.withAPIAuth(s.handleActionCheckUpdate))
	mux.HandleFunc("POST /api/actions/apply-update", s.withAPIAuth(s.handleActionApplyUpdate))
	mux.HandleFunc("POST /api/actions/pair", s.withAPIAuth(s.handleActionPair))
	mux.HandleFunc("POST /api/actions/ingest-start", s.withAPIAuth(s.handleActionIngestStart))
	mux.HandleFunc("POST /api/actions/cancel-ingest", s.withAPIAuth(s.handleActionCancelIngest))
	mux.HandleFunc("GET /api/queue/failed", s.withAPIAuth(s.handleQueueFailed))
	mux.HandleFunc("POST /api/actions/queue-retry", s.withAPIAuth(s.handleActionQueueRetry))
	mux.HandleFunc("POST /api/actions/rollback-update", s.withAPIAuth(s.handleActionRollbackUpdate))
}

func (s *StatusServer) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("tray: api json error: %v", err)
	}
}

func (s *StatusServer) handleAPISettingsGet(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	s.writeJSON(w, http.StatusOK, s.Settings.Snapshot())
}

// settingsPatchRequest is POST /api/settings's body: a single (key, value)
// pair. One key per request, deliberately -- Settings.SetBool/SetInt/
// SetString/SetStringSlice each validate the WHOLE hypothetical config
// before writing (see cmd/branchdam-agent/settings.go's validate*Change
// doc comments), so batching several keys into one call would need its
// own atomic multi-key validate-then-patch, which config.Patch doesn't
// offer today. Value's JSON type selects which Settings setter runs: a
// JSON boolean routes to SetBool, a JSON number to SetInt, a JSON string
// to SetString, a JSON array of strings to SetStringSlice. Per-integration
// path rewrites are not reachable through this generic key/value shape
// (they parse into a structured value, not a plain string) -- see
// POST /api/settings/integration-rewrites instead.
//
// "ingest.cardRoots" and "ingest.allowedExtensions" are reachable through
// BOTH a string (SetString, comma-separated, split and trimmed) and an
// array of strings (SetStringSlice, copied verbatim) -- a caller should
// pick one wire shape per key and stick to it; the array form is the one
// a real list-editing UI should use, since it can't produce a malformed
// comma-separated string in the first place.
type settingsPatchRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func (s *StatusServer) handleAPISettingsPost(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	var req settingsPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}

	var err error
	switch v := req.Value.(type) {
	case bool:
		err = s.Settings.SetBool(req.Key, v)
	case string:
		err = s.Settings.SetString(req.Key, v)
	case float64: // encoding/json decodes every JSON number into float64
		// v != math.Trunc(v) catches a fractional value (int(3.7) would
		// silently truncate to 3); the range check catches a value outside
		// int32 -- both would otherwise wrap/truncate silently through
		// int(v) rather than surfacing as the 400 they should be.
		if v != math.Trunc(v) || v < math.MinInt32 || v > math.MaxInt32 {
			http.Error(w, fmt.Sprintf("value must be a whole number in range for key %q", req.Key), http.StatusBadRequest)
			return
		}
		err = s.Settings.SetInt(req.Key, int(v))
	case []any:
		strs := make([]string, len(v))
		for i, e := range v {
			str, ok := e.(string)
			if !ok {
				http.Error(w, "value must be an array of strings", http.StatusBadRequest)
				return
			}
			strs[i] = str
		}
		err = s.Settings.SetStringSlice(req.Key, strs)
	default:
		http.Error(w, fmt.Sprintf("unsupported value type %T for key %q", v, req.Key), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, http.StatusOK, s.Settings.Snapshot())
}

// idValueSettingsRequest is the body shape shared by
// /api/settings/integration-path and /api/settings/integration-rewrites --
// both select their target purely by integration ID, mirroring
// idActionRequest's own by-ID convention for /api/actions/sync and
// /api/actions/hook-install. Neither route fits settingsPatchRequest's
// generic dotted-key shape: SetIntegrationPath resolves the actual key
// itself (catalogPath vs. databaseUrl), and SetIntegrationRewrites parses
// Value into a structured value SetString never handles.
type idValueSettingsRequest struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func (s *StatusServer) handleAPISettingsIntegrationPath(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	var req idValueSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if err := s.Settings.SetIntegrationPath(IntegrationID(req.ID), req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, http.StatusOK, s.Settings.Snapshot())
}

func (s *StatusServer) handleAPISettingsIntegrationRewrites(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	var req idValueSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if err := s.Settings.SetIntegrationRewrites(IntegrationID(req.ID), req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, http.StatusOK, s.Settings.Snapshot())
}

// pathMappingsSettingsRequest is POST /api/settings/path-mappings' body --
// a structured array rather than idValueSettingsRequest's single string
// value, for the same reason SetIntegrationRewrites needed its own route
// instead of routing through the generic dotted-key one: pathMappings
// parses into a value the generic route never handles, and (unlike
// integration rewrites) the comma/colon string form this key also still
// accepts via POST /api/settings is lossy for a path containing a comma.
type pathMappingsSettingsRequest struct {
	Mappings []PathMappingEntry `json:"mappings"`
}

// handleAPISettingsPathMappings replaces the whole pathMappings list. An
// absent or empty "mappings" array is a legal request -- it's the only way
// to clear the list from this route -- so it is passed through to
// SetPathMappings as an empty (non-nil, per that method's own doc comment)
// slice rather than rejected.
func (s *StatusServer) handleAPISettingsPathMappings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	var req pathMappingsSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	mappings := req.Mappings
	if mappings == nil {
		mappings = []PathMappingEntry{}
	}
	if err := s.Settings.SetPathMappings(mappings); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.writeJSON(w, http.StatusOK, s.Settings.Snapshot())
}

// errString renders err for a JSON response -- encoding/json can't
// usefully marshal an `error`-typed struct field on its own (most error
// implementations expose no exported fields, so a raw json.Marshal would
// silently produce "{}"), so every action result below converts its
// Summary/State's Err via this instead of embedding the summary directly.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type ingestActionRequest struct {
	CardPath string `json:"cardPath"`
}

type ingestActionResult struct {
	CardPath  string    `json:"cardPath"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	ElapsedMs int64     `json:"elapsedMs"`
	Submitted int       `json:"submitted"`
	Resumed   int       `json:"resumed"`
	Skipped   int       `json:"skipped"`
	Failed    int       `json:"failed"`
	Deferred  int       `json:"deferred"`
	Offline   bool      `json:"offline"`
	Err       string    `json:"err,omitempty"`
}

func newIngestActionResult(sum IngestSummary) ingestActionResult {
	return ingestActionResult{
		CardPath:  sum.CardPath,
		StartedAt: sum.StartedAt,
		ElapsedMs: sum.Elapsed.Milliseconds(),
		Submitted: sum.Submitted,
		Resumed:   sum.Resumed,
		Skipped:   sum.Skipped,
		Failed:    sum.Failed,
		Deferred:  sum.Deferred,
		Offline:   sum.Offline,
		Err:       errString(sum.Err),
	}
}

// handleActionIngest runs synchronously to completion before responding --
// TriggerIngest's own doc comment describes the serialization this relies
// on for concurrent-request safety. A real card ingest can run for
// minutes; this route intentionally does not add a client-side timeout of
// its own (net/http.Server has no default handler deadline either), since
// there is no partial-progress result to return early with. A future UI
// phase that wants live progress during a long ingest should poll
// GET /api/status (internal/ingest/progress.go's progress feed already
// flows through Status()) rather than this route returning early.
//
// Every action handler below passes context.WithoutCancel(r.Context()) to
// its Trigger* call, not r.Context() directly -- net/http cancels a
// handler's request context the moment the client disconnects (a closed
// browser tab, an aborted fetch, a network blip), and that has no business
// tearing down a real DualWrite mid-stream. selfupdate.Apply's own doc
// comment makes the identical argument for the identical reason; AGENTS.md
// invariant #15's tray pause gate ("never cancels an in-flight ingest") is
// the other half of it. WithoutCancel deliberately keeps r.Context()'s
// values (there are none here) while dropping only its Done channel/error.
func (s *StatusServer) handleActionIngest(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req ingestActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.CardPath == "" {
		http.Error(w, "cardPath is required", http.StatusBadRequest)
		return
	}
	summary := s.Actions.TriggerIngest(context.WithoutCancel(r.Context()), req.CardPath)
	s.writeJSON(w, http.StatusOK, newIngestActionResult(summary))
}

type drainActionResult struct {
	Ran               bool      `json:"ran"`
	At                time.Time `json:"at,omitempty"`
	HandshakeOK       bool      `json:"handshakeOK,omitempty"`
	LastHandshakeAt   time.Time `json:"lastHandshakeAt,omitempty"`
	NodeCreatedSent   int       `json:"nodeCreatedSent,omitempty"`
	ArchiveCopiesDone int       `json:"archiveCopiesDone,omitempty"`
	RebasesDone       int       `json:"rebasesDone,omitempty"`
	RebasesFailed     int       `json:"rebasesFailed,omitempty"`
	Remaining         int       `json:"remaining,omitempty"`
	Err               string    `json:"err,omitempty"`
}

func newDrainActionResult(sum DrainSummary, ran bool) drainActionResult {
	return drainActionResult{
		Ran:               ran,
		At:                sum.At,
		HandshakeOK:       sum.HandshakeOK,
		LastHandshakeAt:   sum.LastHandshakeAt,
		NodeCreatedSent:   sum.NodeCreatedSent,
		ArchiveCopiesDone: sum.ArchiveCopiesDone,
		RebasesDone:       sum.RebasesDone,
		RebasesFailed:     sum.RebasesFailed,
		Remaining:         sum.Remaining,
		Err:               errString(sum.Err),
	}
}

func (s *StatusServer) handleActionDrain(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), drainPruneClickTimeout)
	defer cancel()
	summary, ran := s.Actions.TriggerDrain(ctx)
	s.writeJSON(w, http.StatusOK, newDrainActionResult(summary, ran))
}

// testConnectionActionResult mirrors drainActionResult's shape -- Ran
// distinguishes "no ServerProbe wired" (nil client, e.g. server.baseUrl/
// apiKey/agentId not yet set) from a real probe outcome, the same
// distinction newDrainActionResult's Ran already draws for a nil Drainer.
type testConnectionActionResult struct {
	Ran     bool      `json:"ran"`
	OK      bool      `json:"ok,omitempty"`
	At      time.Time `json:"at,omitempty"`
	Version string    `json:"version,omitempty"`
	Err     string    `json:"err,omitempty"`
}

func newTestConnectionActionResult(result ProbeResult, ran bool) testConnectionActionResult {
	return testConnectionActionResult{
		Ran:     ran,
		OK:      result.OK,
		At:      result.At,
		Version: result.Version,
		Err:     errString(result.Err),
	}
}

// handleActionTestConnection is the "Test connection" button's endpoint --
// POST /api/actions/test-connection's counterpart to the tray's own
// (future) menu item, mirroring handleActionDrain's shape exactly. Unlike
// /api/actions/drain, this never returns "actions not configured" for a
// merely-incomplete config -- see TriggerServerProbe's own doc comment for
// why a server-reachability check is deliberately not gated on
// ConfigIncomplete: s.Actions itself is only nil when no ActionRunner was
// wired at all (a StatusServer without a live tray), a different, harder
// failure than "the operator hasn't finished setup yet."
func (s *StatusServer) handleActionTestConnection(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	result, ran := s.Actions.TriggerServerProbe(context.WithoutCancel(r.Context()))
	s.writeJSON(w, http.StatusOK, newTestConnectionActionResult(result, ran))
}

// checkUpdateActionResult wraps UpdateStatus's own JSON shape (its
// MarshalJSON already renders Err as a string) rather than defining a
// parallel set of fields -- the Wails window's app.js already reads
// UpdateFound/LatestVersion/Err/etc. off status.selfUpdate, so this
// response parses with the exact same code on the frontend.
type checkUpdateActionResult struct {
	Ran    bool         `json:"ran"`
	Status UpdateStatus `json:"status"`
}

// checkUpdateTimeout bounds handleActionCheckUpdate's call into CheckNow.
// Unlike every other WithoutCancel call site in this file, a self-update
// check's own underlying HTTP client (go-selfupdate's DetectLatest, over
// http.DefaultClient) sets no Timeout of its own -- ctx is the ONLY
// deadline in play, so dropping it via WithoutCancel with nothing to
// replace it would let a connection that establishes but never responds
// block this handler (and hold selfUpdateAgent's checkMu) forever, which
// in turn makes every later manual click AND Run's own interval ticker
// silently report "already running" until a tray restart (Hermes review
// finding on this PR). A single GitHub API call needs far less than this,
// but matches drainPruneClickTimeout's own order of magnitude rather than
// inventing a new one.
const checkUpdateTimeout = 2 * time.Minute

// handleActionCheckUpdate runs (or reports it couldn't run) an on-demand
// self-update check -- read-only (no Runner.TryLockIdle the way
// ApplyLatest needs, since this never touches the installed binary), so
// no request body and no "actions not configured" 503 depends on
// s.Actions at all, only s.Updates. ran=false is not an error: a disabled
// agent, a non-semver build, and a check already in flight all report
// ran=false with the CURRENT UpdateStatus, not a 4xx/5xx.
func (s *StatusServer) handleActionCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if s.Updates == nil {
		http.Error(w, "self-update not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), checkUpdateTimeout)
	defer cancel()
	status, ran := s.Updates.CheckNow(ctx)
	s.writeJSON(w, http.StatusOK, checkUpdateActionResult{Ran: ran, Status: status})
}

type applyUpdateActionResult struct {
	Started bool         `json:"started"`
	Status  UpdateStatus `json:"status"`
}

// handleActionApplyUpdate starts an apply in the background so the Wails
// window remains responsive while the archive and attestations are fetched.
// The caller polls /api/status for phase/error/restart state.
func (s *StatusServer) handleActionApplyUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.UpdateApply == nil {
		http.Error(w, "self-update apply not configured", http.StatusServiceUnavailable)
		return
	}
	status, started := s.UpdateApply.StartApply()
	s.writeJSON(w, http.StatusOK, applyUpdateActionResult{Started: started, Status: status})
}

type pruneActionResult struct {
	Ran        bool      `json:"ran"`
	At         time.Time `json:"at,omitempty"`
	Evaluated  int       `json:"evaluated,omitempty"`
	Pruned     int       `json:"pruned,omitempty"`
	FreedBytes int64     `json:"freedBytes,omitempty"`
	Err        string    `json:"err,omitempty"`
}

func newPruneActionResult(sum PruneSummary, ran bool) pruneActionResult {
	return pruneActionResult{
		Ran:        ran,
		At:         sum.At,
		Evaluated:  sum.Evaluated,
		Pruned:     sum.Pruned,
		FreedBytes: sum.FreedBytes,
		Err:        errString(sum.Err),
	}
}

func (s *StatusServer) handleActionPrune(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), drainPruneClickTimeout)
	defer cancel()
	summary, ran := s.Actions.TriggerPrune(ctx)
	s.writeJSON(w, http.StatusOK, newPruneActionResult(summary, ran))
}

// idActionRequest is the body shape shared by /api/actions/sync and
// /api/actions/hook-install -- both select their target purely by ID.
type idActionRequest struct {
	ID string `json:"id"`
}

type syncActionResult struct {
	Ran           bool      `json:"ran"`
	At            time.Time `json:"at,omitempty"`
	DryRun        bool      `json:"dryRun,omitempty"`
	PairsFound    int       `json:"pairsFound,omitempty"`
	Emitted       int       `json:"emitted,omitempty"`
	Skipped       int       `json:"skipped,omitempty"`
	Errors        int       `json:"errors,omitempty"`
	VirtualNodes  int       `json:"virtualNodes,omitempty"`
	EdgesAttached int       `json:"edgesAttached,omitempty"`
	Removed       int       `json:"removed,omitempty"`
	FileMissing   int       `json:"fileMissing,omitempty"`
	Err           string    `json:"err,omitempty"`
}

func newSyncActionResult(sum SyncSummary, ran bool) syncActionResult {
	return syncActionResult{
		Ran:           ran,
		At:            sum.At,
		DryRun:        sum.DryRun,
		PairsFound:    sum.PairsFound,
		Emitted:       sum.Emitted,
		Skipped:       sum.Skipped,
		Errors:        sum.Errors,
		VirtualNodes:  sum.VirtualNodes,
		EdgesAttached: sum.EdgesAttached,
		Removed:       sum.Removed,
		FileMissing:   sum.FileMissing,
		Err:           errString(sum.Err),
	}
}

func (s *StatusServer) handleActionSync(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req idActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	summary, ran := s.Actions.TriggerSync(context.WithoutCancel(r.Context()), IntegrationID(req.ID))
	s.writeJSON(w, http.StatusOK, newSyncActionResult(summary, ran))
}

type hookInstallActionResult struct {
	Ran       bool      `json:"ran"`
	At        time.Time `json:"at,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Path      string    `json:"path,omitempty"`
	Installed bool      `json:"installed,omitempty"`
	UpToDate  bool      `json:"upToDate,omitempty"`
	Err       string    `json:"err,omitempty"`
}

func newHookInstallActionResult(state HookState, ran bool) hookInstallActionResult {
	return hookInstallActionResult{
		Ran:       ran,
		At:        state.At,
		Dir:       state.Dir,
		Path:      state.Path,
		Installed: state.Installed,
		UpToDate:  state.UpToDate,
		Err:       errString(state.Err),
	}
}

func (s *StatusServer) handleActionHookInstall(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req idActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	state, ran := s.Actions.TriggerHookInstall(context.WithoutCancel(r.Context()), HookID(req.ID))
	s.writeJSON(w, http.StatusOK, newHookInstallActionResult(state, ran))
}

// hookRevealActionResult is deliberately just an Err field -- RevealHook
// itself returns nothing to report on success (see its own doc comment:
// "fire-and-forget", no state mutation), so there is no summary shape to
// mirror the way newHookInstallActionResult mirrors HookState.
type hookRevealActionResult struct {
	Err string `json:"err,omitempty"`
}

// handleActionHookReveal has no context.WithoutCancel call, unlike every
// other action handler in this file: RevealHook takes no ctx parameter at
// all (it's a synchronous, near-instant OS shell-out -- see its own doc
// comment), so there is nothing here for a canceled request context to cut
// short in the first place.
func (s *StatusServer) handleActionHookReveal(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req idActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	err := s.Actions.RevealHook(HookID(req.ID))
	s.writeJSON(w, http.StatusOK, hookRevealActionResult{Err: errString(err)})
}

// pauseActionRequest/-Result drive the SESSION-ONLY pause gate
// (Runner.Paused/SetPaused -- AGENTS.md invariant #15's "paused" gate),
// never the persisted ingest.pauseUploadOnMetered config key. The two are
// deliberately independent gates with different persistence semantics;
// conflating them behind one route would violate that invariant. A route
// for the persisted metered-network gate belongs on /api/settings instead
// (key "ingest.pauseUploadOnMetered", a JSON boolean value), going through
// Settings.SetBool like every other persisted config change.
type pauseActionRequest struct {
	Paused bool `json:"paused"`
}

type pauseActionResult struct {
	Paused bool `json:"paused"`
}

func (s *StatusServer) handleActionPause(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req pauseActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.Actions.SetPaused(req.Paused)
	s.writeJSON(w, http.StatusOK, pauseActionResult{Paused: s.Actions.Paused()})
}

type pairActionRequest struct {
	URL string `json:"url"`
	// Source is PairSourceDeepLink when the URL came from an OS protocol
	// handoff; such requests always require operator confirmation.
	Source string `json:"source,omitempty"`
}

type pairActionResult struct {
	OK bool `json:"ok"`
	// Pending is set for deep-link requests: the operator confirmation and
	// pairing continue in the tray after this response.
	Pending bool   `json:"pending,omitempty"`
	Server  string `json:"server"`
	AgentID string `json:"agentId"`
}

func (s *StatusServer) handleActionPair(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		http.Error(w, "settings not configured", http.StatusServiceUnavailable)
		return
	}
	var req pairActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	parsed, err := branchdam.ParsePairingURL(req.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Source == PairSourceDeepLink {
		// The operator confirmation dialog can outlive the caller's HTTP
		// timeout, so confirm + pair run detached and report through the
		// tray notification (handleDeepLink). The URL is already validated.
		if s.PairConfirm == nil {
			http.Error(w, ErrPairNoConfirm.Error(), http.StatusServiceUnavailable)
			return
		}
		go submitDeepLink(context.Background(), req.URL, s.Settings, s.PairConfirm, s.PairNotify)
		s.writeJSON(w, http.StatusAccepted, pairActionResult{OK: true, Pending: true, Server: parsed.Server, AgentID: parsed.Agent})
		return
	}
	err = s.Settings.Pair(parsed.Server, parsed.Key, parsed.Agent)
	if err != nil {
		// Differentiate client/credential problems (400) from internal
		// errors (500) via typed errors only -- never message text:
		// "config problem: ..." is emitted BOTH by pairConfig's
		// pre-write validation (wrapped in ErrPairingConfigInvalid, 400)
		// and by the post-patch reload (settings.go, a genuine 500 even
		// though the file was already written). ErrPairingHelloFailed
		// covers the credential-validation call (rejected key via
		// *HTTPError, or dead/unreachable host via a bare network error)
		// -- both client input. Everything else -- perms refusal,
		// I/O, reload failure -- is server-side (500).
		status := http.StatusInternalServerError
		var httpErr *branchdam.HTTPError
		if errors.Is(err, branchdam.ErrPairingURLInvalid) ||
			errors.Is(err, branchdam.ErrPairingConfigInvalid) ||
			errors.Is(err, branchdam.ErrPairingAgentBlank) ||
			errors.Is(err, branchdam.ErrPairingHelloFailed) ||
			errors.As(err, &httpErr) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	s.writeJSON(w, http.StatusOK, pairActionResult{
		OK:      true,
		Server:  parsed.Server,
		AgentID: parsed.Agent,
	})
}

// ingestStartRequest is POST /api/actions/ingest-start's body. An empty
// CardPath means "Ingest now": every card volume IngestNowVolumes finds,
// exactly the tray menu item's selection. A non-empty CardPath is "Import
// from folder…": that one folder, picked in the window.
type ingestStartRequest struct {
	CardPath string `json:"cardPath"`
}

// ingestStartResult reports whether a batch was started. Started=false is
// not an HTTP error: paused, config incomplete, an ingest already running,
// nothing to ingest, and a rejected folder all come back as 200 with Err
// saying which, so the window can show it next to the button.
type ingestStartResult struct {
	Started   bool     `json:"started"`
	CardPaths []string `json:"cardPaths,omitempty"`
	Err       string   `json:"err,omitempty"`
}

// errIngestAlreadyRunning is ingest-start's refusal while an ingest (from
// any path: detector, menu, or an earlier window click) is in flight.
var errIngestAlreadyRunning = errors.New("an ingest is already running -- wait for it to finish or cancel it")

// errGateHeld is ingest-start's refusal while something other than an
// ingest holds the gate: a prune pass (TriggerPrune's TryLockIdle) or a
// self-update download, apply or roll back. A drain does not take it.
var errGateHeld = errors.New("a prune or update is in progress -- try again when it finishes")

// errFolderPath is ingest-start's refusal for a folder path that is not
// absolute or contains "..": the window's folder picker always returns a
// clean absolute path, so anything else did not come from it.
var errFolderPath = errors.New("pick the folder with Import from folder... (the path must be absolute, without \"..\")")

// handleActionIngestStart starts an ingest batch and returns at once
// (202): unlike POST /api/actions/ingest (synchronous, kept for scripted
// callers), a card ingest runs for minutes and the window must stay
// responsive -- it follows progress through GET /api/status, as it
// already does for detector-started ingests. Every volume goes through
// Runner.TriggerIngest, so the batch is serialized behind Runner.gate with
// every other ingest and self-update (invariant 9) and honours the pause
// gate; a manual ingest bypasses the IngestGate dialog and never
// auto-ejects, the same as the tray menu's "Ingest now" and "Import from
// folder…". A batch stops early when a volume's ingest reports Canceled
// (POST /api/actions/cancel-ingest), so one cancel stops the batch.
func (s *StatusServer) handleActionIngestStart(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req ingestStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	refuse := func(err error) {
		s.writeJSON(w, http.StatusOK, ingestStartResult{Err: err.Error()})
	}
	if s.Actions.Paused() {
		refuse(errors.New("ingest is paused -- resume it first"))
		return
	}
	if s.Actions.ConfigIncomplete() {
		refuse(ErrConfigIncomplete)
		return
	}
	if _, _, busy := s.Actions.Busy(); busy || s.ingestBatch.Load() {
		refuse(errIngestAlreadyRunning)
		return
	}
	// Busy covers only an ingest; a self-update holds the same gate
	// (TryLockIdle) for its whole download-and-apply window. Accepting a
	// batch then would block it on the gate behind the update, whose
	// restart drops it -- so refuse up front, like the update path does.
	if s.Actions.GateHeld() {
		refuse(errGateHeld)
		return
	}

	var paths []string
	if req.CardPath == "" {
		paths = s.Actions.IngestNowVolumes()
		if len(paths) == 0 {
			refuse(errors.New(ingestNowEmptyMessage(s.Actions.WatchDirs())))
			return
		}
	} else {
		// The path comes from the request: accept only what the window's
		// native folder picker produces -- an absolute path with no ".."
		// -- before it reaches the filesystem. The substring test is the
		// form CodeQL's go/path-injection recognises as a sanitizer; it
		// also refuses a folder whose name holds "..", which the refusal
		// message names.
		if !filepath.IsAbs(req.CardPath) || strings.Contains(req.CardPath, "..") {
			refuse(errFolderPath)
			return
		}
		cardPath := filepath.Clean(req.CardPath)
		// Same refusal as the tray menu's "Import from folder…"
		// (handleImportFolder): a watched cardRoots parent holds every
		// mounted disk, the NAS and the edit SSD included.
		for _, dir := range s.Actions.WatchDirs() {
			if samePath(dir, cardPath) {
				refuse(errors.New("already ingesting this path -- pick a card or folder inside it"))
				return
			}
		}
		info, err := os.Stat(cardPath)
		if err != nil || !info.IsDir() {
			refuse(fmt.Errorf("%s is not a folder that can be read", cardPath))
			return
		}
		paths = []string{cardPath}
	}

	if !s.ingestBatch.CompareAndSwap(false, true) {
		refuse(errIngestAlreadyRunning)
		return
	}
	// Detached from the request like every other action handler (a closed
	// window must not stop a card mid-copy); the operator's way to stop it
	// is CancelIngest.
	ctx := context.WithoutCancel(r.Context())
	go func() {
		defer s.ingestBatch.Store(false)
		for _, p := range paths {
			// The gate is released between volumes, so a prune or update
			// can take it mid-batch. TriggerIngestNoWait acquires it
			// without waiting -- check and acquire in one step -- and the
			// batch ends instead of blocking behind the holder, where
			// CancelIngest can't reach it (and an update's restart would
			// drop it). The window shows the batch as finished and the
			// operator can start it again.
			sum := s.Actions.TriggerIngestNoWait(ctx, p)
			if errors.Is(sum.Err, ErrGateBusy) {
				log.Printf("tray: ingest-start: batch stopped: %v", errGateHeld)
				return
			}
			if sum.Canceled {
				return
			}
		}
	}()
	s.writeJSON(w, http.StatusAccepted, ingestStartResult{Started: true, CardPaths: paths})
}

type cancelIngestActionResult struct {
	// Canceled is false when no ingest was in flight to stop.
	Canceled bool `json:"canceled"`
}

// handleActionCancelIngest asks the in-flight ingest to stop after its
// current file (Runner.CancelIngest). It takes no gate and returns at
// once; the stop shows up in GET /api/status as ingestCancelRequested and
// then as lastIngest.canceled.
func (s *StatusServer) handleActionCancelIngest(w http.ResponseWriter, _ *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	s.writeJSON(w, http.StatusOK, cancelIngestActionResult{Canceled: s.Actions.CancelIngest()})
}

// failedRowsDefaultLimit caps GET /api/queue/failed when the caller names
// no limit: a list the window renders in full, while Counts().Failed (in
// GET /api/status) still reports the true total.
const failedRowsDefaultLimit = 200

// failedRowsMaxLimit is the largest ?limit= GET /api/queue/failed accepts:
// the store scan and the response body grow with it.
const failedRowsMaxLimit = 1000

type queueFailedResult struct {
	// Configured is false when no offline queue is wired; Rows is then
	// empty, which is not an error.
	Configured bool             `json:"configured"`
	Rows       []FailedQueueRow `json:"rows"`
}

// handleQueueFailed lists FAILED queue rows for the window's failed-rows
// list -- read-only, but behind the same origin and token checks as
// every other /api/* route, since a row carries local file paths.
// ?limit=N (1..failedRowsMaxLimit) overrides failedRowsDefaultLimit.
func (s *StatusServer) handleQueueFailed(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	limit := failedRowsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > failedRowsMaxLimit {
			http.Error(w, fmt.Sprintf("limit must be a whole number from 1 to %d", failedRowsMaxLimit), http.StatusBadRequest)
			return
		}
		limit = n
	}
	rows, err := s.Actions.FailedQueueRows(r.Context(), limit)
	switch {
	case errors.Is(err, ErrQueueNotConfigured):
		s.writeJSON(w, http.StatusOK, queueFailedResult{Rows: []FailedQueueRow{}})
		return
	case errors.Is(err, ErrQueueRetryUnsupported):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []FailedQueueRow{}
	}
	s.writeJSON(w, http.StatusOK, queueFailedResult{Configured: true, Rows: rows})
}

// queueRetryRequest is POST /api/actions/queue-retry's body: exactly one
// of NodeUUID (retry that row) or All (retry every FAILED row, what
// `queue-drain -retry-failed` does). An empty body is rejected rather
// than read as "all", so a malformed per-row click can never re-queue
// every failed row.
type queueRetryRequest struct {
	NodeUUID string `json:"nodeUuid"`
	All      bool   `json:"all"`
}

// handleActionQueueRetry re-queues FAILED rows through
// Runner.RetryFailedQueueRows (queue.Store.ResetFailedNode/ResetFailed).
// It does not start a drain: the next timer pass, or the window's "Drain
// now", retries them -- under the pause and metered-connection gates
// TriggerDrain already applies.
func (s *StatusServer) handleActionQueueRetry(w http.ResponseWriter, r *http.Request) {
	if s.Actions == nil {
		http.Error(w, "actions not configured", http.StatusServiceUnavailable)
		return
	}
	var req queueRetryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if (req.NodeUUID == "") == !req.All {
		http.Error(w, "exactly one of nodeUuid or all is required", http.StatusBadRequest)
		return
	}
	counts, err := s.Actions.RetryFailedQueueRows(r.Context(), req.NodeUUID)
	switch {
	case errors.Is(err, ErrQueueNotConfigured), errors.Is(err, ErrQueueRetryUnsupported):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, http.StatusOK, counts)
}

type rollbackUpdateActionResult struct {
	Started bool         `json:"started"`
	Status  UpdateStatus `json:"status"`
}

// handleActionRollbackUpdate starts a rollback in the background, the
// same shape as handleActionApplyUpdate: the window polls /api/status for
// the rolling-back and restarting phases (or Err).
func (s *StatusServer) handleActionRollbackUpdate(w http.ResponseWriter, _ *http.Request) {
	if s.UpdateRollback == nil {
		http.Error(w, "self-update rollback not configured", http.StatusServiceUnavailable)
		return
	}
	status, started := s.UpdateRollback.StartRollback()
	s.writeJSON(w, http.StatusOK, rollbackUpdateActionResult{Started: started, Status: status})
}
