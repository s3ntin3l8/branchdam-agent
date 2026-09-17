package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// statusPageView is what handleStatusJSON renders -- deliberately a
// superset of Status with presentation-only fields (Version), not a second
// source of truth: it always derives from a live Status()/Settings() call.
// Settings is CONFIG state (enabled, dry run, catalog path) that Status
// deliberately never carries -- see Status.Integrations' own doc comment.
//
// This used to also be the browsable HTML status page's view model (a
// hand-maintained Go template at assets/index.html, since removed): it
// duplicated the Wails window's Settings/status rendering in a second
// templating language with no consumer of its own, so it was dropped in
// favor of the window as the single settings/status surface. Every route
// this server exposes now serves this same JSON shape -- see mux.
type statusPageView struct {
	Version  string       `json:"version"`
	Status   Status       `json:"status"`
	Settings SettingsView `json:"settings,omitempty"`
}

// StatusServer is the embedded localhost status page: spec §7.2's three
// things (watch directories, scratch usage, queue status), the last thing
// the M1 gate needs since the queue is a stub until M2. StatusFunc is
// called fresh on every request -- there is no caching layer here, this is
// a single operator's own workstation, not a service under load.
type StatusServer struct {
	// Addr is normalized to a loopback-only address by NewStatusServer --
	// see normalizeLoopback's doc comment for why a bare ":port" is
	// rewritten rather than trusted as-is.
	Addr       string
	StatusFunc func() Status
	// SettingsFunc supplies the CONFIG-state half of the Integrations
	// section (enabled, dry run, catalog path) -- nil-tolerant, since
	// several existing tests construct a StatusServer as a bare struct
	// literal with no settings source; handleStatusJSON renders an empty
	// SettingsView in that case rather than calling a nil func.
	SettingsFunc func() SettingsView
	Version      string

	// Actions, Settings, and Token gate the hardened /api/* surface (see
	// statusapi.go) -- all three are nil-tolerant, matching SettingsFunc's
	// own precedent: a StatusServer built without them (as most existing
	// tests still do, and as any caller not wiring the API in yet) serves
	// the legacy read-only page/status endpoints exactly as before, and
	// registerAPIRoutes's handlers each 503 rather than panic when Actions
	// or Settings is nil.
	Actions  ActionRunner
	Settings Settings
	// Updates gates the one on-demand self-update actuator
	// (POST /api/actions/check-update) -- nil-tolerant like Actions/
	// Settings above, so a StatusServer that doesn't wire it just 503s
	// that one route rather than panicking. A separate field/interface
	// from Actions on purpose: Runner (ActionRunner's implementation) has
	// no self-update knowledge at all, and growing tray.SelfUpdater (the
	// tray menu's own contract for ApplyLatest/Rollback) to add this
	// status-API concern would churn every implementation and fake of
	// that interface for something they don't need.
	Updates UpdateChecker
	// Token, when set, is the shared secret every /api/* request must
	// present as "Authorization: Bearer <Token>" -- see
	// internal/sessiontoken.Generate and tokenValid. Left empty, every
	// /api/* request is rejected (fail closed), never "auth disabled".
	Token string

	srv *http.Server
}

// NewStatusServer builds a StatusServer bound to addr (or its loopback-only
// rewrite -- see normalizeLoopback). statusFunc and settingsFunc are each
// called once per request.
func NewStatusServer(addr string, statusFunc func() Status, settingsFunc func() SettingsView, version string) *StatusServer {
	return &StatusServer{Addr: normalizeLoopback(addr), StatusFunc: statusFunc, SettingsFunc: settingsFunc, Version: version}
}

// normalizeLoopback rewrites a bare ":port" (which net/http would bind to
// every interface) to "127.0.0.1:port". The status page renders local
// filesystem paths and, once M2 lands, queue depth -- there is no reason
// for it to ever be reachable from off the workstation, and binding wide
// open is exactly the kind of thing a Go security scanner (this repo runs
// CodeQL as a required check) flags for good reason. An already-explicit
// host (including a deliberate "0.0.0.0:port") is left untouched.
func normalizeLoopback(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if addr == "" {
		return "127.0.0.1:38080"
	}
	return addr
}

// Listen binds s.Addr and returns the listener without serving anything
// yet. Split out from ListenAndServe/Serve so a caller can treat the bind
// itself as a single-instance guard: call Listen before starting anything
// else, and a bind failure ("address already in use") means another tray
// process already holds this port. This is also what makes a self-update
// relaunch safe -- the successor cannot bind here until this listener is
// closed by Serve's shutdown, so a caller must not spawn the successor
// until Serve has returned.
func (s *StatusServer) Listen() (net.Listener, error) {
	return net.Listen("tcp", s.Addr)
}

// mux builds this server's route table. Split out from Serve so tests can
// exercise routing (root vs. unknown paths, method matching) without
// binding a real listener.
func (s *StatusServer) mux() *http.ServeMux {
	mux := http.NewServeMux()
	// All three routes serve the same JSON status view (see statusPageView's
	// doc comment) -- there is no browsable HTML page any more. "/{$}" is
	// the exact-root pattern (Go 1.22+ ServeMux), replacing the old
	// handleIndex's manual "path != /" 404 guard with routing: any other
	// path 404s automatically instead of matching this handler.
	mux.HandleFunc("/{$}", s.handleStatusJSON)
	mux.HandleFunc("/status", s.handleStatusJSON)
	mux.HandleFunc("/status.json", s.handleStatusJSON)
	s.registerAPIRoutes(mux)
	return mux
}

// Serve runs the status server on ln and blocks until ctx is cancelled,
// then shuts down gracefully. Returns nil on a clean ctx-triggered
// shutdown, matching net/http.Server.Shutdown's own contract.
func (s *StatusServer) Serve(ctx context.Context, ln net.Listener) error {
	s.srv = &http.Server{
		Handler:           s.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ListenAndServe binds s.Addr and serves until ctx is cancelled -- a thin
// wrapper over Listen+Serve for callers (and existing tests) that don't
// need the single-instance guard split out.
func (s *StatusServer) ListenAndServe(ctx context.Context) error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

func (s *StatusServer) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	view := statusPageView{Version: s.Version, Status: s.StatusFunc()}
	if s.SettingsFunc != nil {
		view.Settings = s.SettingsFunc()
	}
	if err := json.NewEncoder(w).Encode(view); err != nil {
		log.Printf("tray: status json error: %v", err)
	}
}

// StatusURL returns the http:// URL this server's Addr resolves to, for
// logging and diagnostics. There is no tray menu item that opens it any
// more -- the Wails window is the only status/settings surface -- but the
// JSON endpoint itself (see Serve) is still live for anyone who wants to
// curl it.
func (s *StatusServer) StatusURL() string {
	return fmt.Sprintf("http://%s/", s.Addr)
}
