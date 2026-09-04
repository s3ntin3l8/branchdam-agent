package tray

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

//go:embed assets/index.html
var statusPageFS embed.FS

var statusPageTmpl = template.Must(template.New("index.html").Funcs(template.FuncMap{
	"since": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	// integrationView bridges SettingsView.Integration's (T, bool) return
	// into the single value text/template can call: a template method call
	// must return exactly one value, or two where the second is `error` --
	// (IntegrationView, bool) doesn't qualify. A miss (ok=false) renders as
	// IntegrationView{}'s zero value -- an intentional fallback, not a template
	// bug: cmd/branchdam-agent's TestRegistryCompleteness guarantees
	// Settings.Snapshot() emits one entry per Integrations() registry ID, so
	// a real miss can't happen in this repo's own wiring. A third-party
	// Settings implementation that violated that bijection would render as
	// "disabled / live / catalog not set" rather than fail loudly -- an
	// accepted trade, since a status page has no error-reporting surface
	// beyond its own sections (see settingsmenu.go's lastErr for where
	// config-change errors actually surface).
	"integrationView": func(sv SettingsView, id IntegrationID) IntegrationView {
		iv, _ := sv.Integration(id)
		return iv
	},
}).ParseFS(statusPageFS, "assets/index.html"))

// statusPageView is what statusPageTmpl renders -- deliberately a superset
// of Status with presentation-only fields (Version), not a second source
// of truth: PageData always derives from a live Status()/Settings()
// call. Settings is CONFIG state (enabled, dry run, catalog path) that
// Status deliberately never carries -- see Status.Integrations' own doc
// comment -- so the Integrations section joins the two by ID via the
// integrationView template func above.
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
	// literal with no settings source; handleIndex renders an empty
	// SettingsView in that case rather than calling a nil func.
	SettingsFunc func() SettingsView
	Version      string

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

// Serve runs the status page on ln and blocks until ctx is cancelled, then
// shuts down gracefully. Returns nil on a clean ctx-triggered shutdown,
// matching net/http.Server.Shutdown's own contract.
func (s *StatusServer) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/status", s.handleStatusJSON)
	mux.HandleFunc("/status.json", s.handleStatusJSON)

	s.srv = &http.Server{
		Handler:           mux,
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

func (s *StatusServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.handleStatusJSON(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := statusPageView{Version: s.Version, Status: s.StatusFunc()}
	if s.SettingsFunc != nil {
		view.Settings = s.SettingsFunc()
	}
	if err := statusPageTmpl.Execute(w, view); err != nil {
		// Template execution failing mid-write can't be turned into a
		// clean error response (headers/some body bytes may already be
		// flushed) -- log and move on, matching net/http's own
		// recommendation for this exact situation.
		log.Printf("tray: status page template error: %v", err)
	}
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
// display in the tray menu and for "Open status page".
func (s *StatusServer) StatusURL() string {
	return fmt.Sprintf("http://%s/", s.Addr)
}
