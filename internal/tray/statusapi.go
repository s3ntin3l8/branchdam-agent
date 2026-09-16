package tray

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"time"
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
type ActionRunner interface {
	TriggerIngest(ctx context.Context, cardPath string) IngestSummary
	TriggerDrain(ctx context.Context) (DrainSummary, bool)
	TriggerPrune(ctx context.Context) (PruneSummary, bool)
	TriggerSync(ctx context.Context, id IntegrationID) (SyncSummary, bool)
	TriggerHookInstall(ctx context.Context, id HookID) (HookState, bool)
	TriggerServerProbe(ctx context.Context) (ProbeResult, bool)
	RevealHook(id HookID) error
	Paused() bool
	SetPaused(v bool)
}

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
	if host == "localhost" {
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
	mux.HandleFunc("POST /api/actions/ingest", s.withAPIAuth(s.handleActionIngest))
	mux.HandleFunc("POST /api/actions/drain", s.withAPIAuth(s.handleActionDrain))
	mux.HandleFunc("POST /api/actions/prune", s.withAPIAuth(s.handleActionPrune))
	mux.HandleFunc("POST /api/actions/sync", s.withAPIAuth(s.handleActionSync))
	mux.HandleFunc("POST /api/actions/hook-install", s.withAPIAuth(s.handleActionHookInstall))
	mux.HandleFunc("POST /api/actions/hook-reveal", s.withAPIAuth(s.handleActionHookReveal))
	mux.HandleFunc("POST /api/actions/pause", s.withAPIAuth(s.handleActionPause))
	mux.HandleFunc("POST /api/actions/test-connection", s.withAPIAuth(s.handleActionTestConnection))
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
	Skipped   int       `json:"skipped"`
	Failed    int       `json:"failed"`
	Offline   bool      `json:"offline"`
	Err       string    `json:"err,omitempty"`
}

func newIngestActionResult(sum IngestSummary) ingestActionResult {
	return ingestActionResult{
		CardPath:  sum.CardPath,
		StartedAt: sum.StartedAt,
		ElapsedMs: sum.Elapsed.Milliseconds(),
		Submitted: sum.Submitted,
		Skipped:   sum.Skipped,
		Failed:    sum.Failed,
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
	summary, ran := s.Actions.TriggerDrain(context.WithoutCancel(r.Context()))
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
	summary, ran := s.Actions.TriggerPrune(context.WithoutCancel(r.Context()))
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
