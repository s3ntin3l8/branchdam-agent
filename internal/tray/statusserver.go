package tray

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed assets/index.html
var statusPageFS embed.FS

var statusPageTmpl = template.Must(template.New("index.html").Funcs(template.FuncMap{
	"since": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
}).ParseFS(statusPageFS, "assets/index.html"))

// statusPageView is what statusPageTmpl renders -- deliberately a superset
// of Status with presentation-only fields (Version), not a second source
// of truth: PageData always derives from a live Status() call.
type statusPageView struct {
	Version string
	Status  Status
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
	Version    string

	srv *http.Server
}

// NewStatusServer builds a StatusServer bound to addr (or its loopback-only
// rewrite -- see normalizeLoopback). statusFunc is called once per request.
func NewStatusServer(addr string, statusFunc func() Status, version string) *StatusServer {
	return &StatusServer{Addr: normalizeLoopback(addr), StatusFunc: statusFunc, Version: version}
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

// ListenAndServe starts the status page and blocks until ctx is cancelled,
// then shuts down gracefully. Returns nil on a clean ctx-triggered
// shutdown, matching net/http.Server.Shutdown's own contract.
func (s *StatusServer) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)

	s.srv = &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.ListenAndServe()
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

func (s *StatusServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	view := statusPageView{Version: s.Version, Status: s.StatusFunc()}
	if err := statusPageTmpl.Execute(w, view); err != nil {
		// Template execution failing mid-write can't be turned into a
		// clean error response (headers/some body bytes may already be
		// flushed) -- log and move on, matching net/http's own
		// recommendation for this exact situation.
		log.Printf("tray: status page template error: %v", err)
	}
}

// StatusURL returns the http:// URL this server's Addr resolves to, for
// display in the tray menu and for "Open status page".
func (s *StatusServer) StatusURL() string {
	return fmt.Sprintf("http://%s/", s.Addr)
}
