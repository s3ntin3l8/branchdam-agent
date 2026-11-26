package mcpserver

import (
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

func TestActionToolsHitRoutes(t *testing.T) {
	withTempTokenDir(t)
	token := writeToken(t, "token-one")

	type hit struct {
		method string
		path   string
		host   string
		auth   string
		origin string
		site   string
		body   string
	}
	var hits []hit
	statusAddr := "127.0.0.1:38080"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		raw, err := readBody(r)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		hits = append(hits, hit{
			method: r.Method,
			path:   r.URL.Path,
			host:   r.Host,
			auth:   r.Header.Get("Authorization"),
			origin: r.Header.Get("Origin"),
			site:   r.Header.Get("Sec-Fetch-Site"),
			body:   string(raw),
		})
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case "/api/actions/ingest-start":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"started":true}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	opts := Options{StatusAddr: statusAddr, trayBaseURL: srv.URL, GOOS: "darwin"}
	calls := []struct {
		name string
		args map[string]any
		path string
		body string
	}{
		{name: "tray_status", path: "/api/status"},
		{name: "start_ingest", args: map[string]any{"cardPath": "/mnt/card"}, path: "/api/actions/ingest-start", body: `{"cardPath":"/mnt/card"}`},
		{name: "cancel_ingest", path: "/api/actions/cancel-ingest"},
		{name: "drain_now", path: "/api/actions/drain"},
		{name: "retry_failed", args: map[string]any{"nodeUuid": "node-1"}, path: "/api/actions/queue-retry", body: `{"nodeUuid":"node-1"}`},
		{name: "set_paused", args: map[string]any{"paused": true}, path: "/api/actions/pause", body: `{"paused":true}`},
	}
	for _, call := range calls {
		res := callTool(t, opts, call.name, call.args)
		if res.IsError {
			t.Fatalf("%s: %s", call.name, toolText(res))
		}
	}
	if len(hits) != len(calls) {
		t.Fatalf("hits = %d, want %d (%#v)", len(hits), len(calls), hits)
	}
	for i, call := range calls {
		got := hits[i]
		method := http.MethodPost
		if call.name == "tray_status" {
			method = http.MethodGet
		}
		if got.method != method || got.path != call.path {
			t.Errorf("%s: %s %s, want %s %s", call.name, got.method, got.path, method, call.path)
		}
		if got.host != statusAddr {
			t.Errorf("%s: Host = %q, want %q", call.name, got.host, statusAddr)
		}
		if got.auth != "Bearer "+token {
			t.Errorf("%s: Authorization = %q", call.name, got.auth)
		}
		if got.origin != "" || got.site != "" {
			t.Errorf("%s: Origin %q Sec-Fetch-Site %q, want empty", call.name, got.origin, got.site)
		}
		if call.body != "" && got.body != call.body {
			t.Errorf("%s: body = %s, want %s", call.name, got.body, call.body)
		}
	}

	res := callTool(t, opts, "retry_failed", map[string]any{"all": true})
	if res.IsError {
		t.Fatal(toolText(res))
	}
	last := hits[len(hits)-1]
	if last.path != "/api/actions/queue-retry" || last.body != `{"all":true}` {
		t.Fatalf("all retry: %+v", last)
	}
	if last.auth != "Bearer "+token {
		t.Fatalf("all retry auth = %q", last.auth)
	}
}

func TestTokenRereadAfterRewrite(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-old")
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		auths = append(auths, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "127.0.0.1:38080", trayBaseURL: srv.URL, GOOS: "windows"}

	if res := callTool(t, opts, "tray_status", nil); res.IsError {
		t.Fatal(toolText(res))
	}
	writeToken(t, "token-new")
	if res := callTool(t, opts, "tray_status", nil); res.IsError {
		t.Fatal(toolText(res))
	}
	if len(auths) != 2 || auths[0] != "Bearer token-old" || auths[1] != "Bearer token-new" {
		t.Fatalf("auths = %#v", auths)
	}
}

func TestTrayNotRunning(t *testing.T) {
	withTempTokenDir(t)
	opts := Options{StatusAddr: "127.0.0.1:38080", GOOS: "darwin"}
	res := callTool(t, opts, "drain_now", nil)
	if res.IsError || toolText(res) != trayNotRunning {
		t.Fatalf("missing token: error=%v text=%q", res.IsError, toolText(res))
	}

	writeToken(t, "token-present")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	opts.StatusAddr = addr
	res = callTool(t, opts, "tray_status", nil)
	if res.IsError || toolText(res) != trayNotRunning {
		t.Fatalf("refused: error=%v text=%q", res.IsError, toolText(res))
	}
}

func TestRetryFailedDoesNotCallTray(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		hits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "127.0.0.1:38080", trayBaseURL: srv.URL, GOOS: "darwin", ReadToken: func() (string, error) {
		return "unused", nil
	}}
	for _, args := range []map[string]any{
		nil,
		{},
		{"nodeUuid": "abc", "all": true},
		{"nodeUuid": "  ", "all": false},
	} {
		res := callTool(t, opts, "retry_failed", args)
		if !res.IsError {
			t.Fatalf("args %#v: want error, got %s", args, toolText(res))
		}
		if !strings.Contains(toolText(res), "exactly one") {
			t.Fatalf("args %#v: %s", args, toolText(res))
		}
	}
	if hits != 0 {
		t.Fatalf("tray hits = %d, want 0", hits)
	}
}

func TestLinuxTrayUnsupported(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		hits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	opts := Options{
		StatusAddr:  "127.0.0.1:38080",
		trayBaseURL: srv.URL,
		GOOS:        "linux",
		ReadToken: func() (string, error) {
			t.Error("token read on linux")
			return "", os.ErrNotExist
		},
	}
	res := callTool(t, opts, "start_ingest", nil)
	if res.IsError || toolText(res) != trayUnsupported {
		t.Fatalf("error=%v text=%q", res.IsError, toolText(res))
	}
	if hits != 0 {
		t.Fatalf("tray hits = %d", hits)
	}
}

func TestTrayHTTPError(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-one")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		http.Error(w, "ingest is paused", http.StatusConflict)
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "127.0.0.1:38080", trayBaseURL: srv.URL, GOOS: "darwin"}
	res := callTool(t, opts, "cancel_ingest", nil)
	if !res.IsError || !strings.Contains(toolText(res), "ingest is paused") {
		t.Fatalf("error=%v text=%q", res.IsError, toolText(res))
	}
}

func TestTrayDoesNotFollowRedirect(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-one")
	var leaked []string
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		leaked = append(leaked, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer next.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		http.Redirect(w, r, next.URL+"/api/status", http.StatusFound)
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "127.0.0.1:38080", trayBaseURL: srv.URL, GOOS: "darwin"}
	res := callTool(t, opts, "tray_status", nil)
	if !res.IsError {
		t.Fatal("redirect was treated as success")
	}
	if len(leaked) != 0 {
		t.Fatalf("token forwarded: %#v", leaked)
	}
}

func TestNonLoopbackStatusAddr(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		hits++
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "192.0.2.10:38080", trayBaseURL: srv.URL, GOOS: "darwin", ReadToken: func() (string, error) {
		t.Error("token read for a non-loopback address")
		return "not-sent", nil
	}}
	res := callTool(t, opts, "tray_status", nil)
	if !res.IsError || hits != 0 {
		t.Fatalf("error=%v hits=%d text=%q", res.IsError, hits, toolText(res))
	}
}

func TestResolveStatusAddrEachCall(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-one")
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		hosts = append(hosts, r.Host)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	n := 0
	addrs := []string{":38080", "127.0.0.1:38081"}
	opts := Options{
		StatusAddr:  "192.0.2.10:1",
		trayBaseURL: srv.URL,
		GOOS:        "darwin",
		ResolveStatusAddr: func() (string, error) {
			addr := addrs[n]
			n++
			return addr, nil
		},
	}
	for range 2 {
		res := callTool(t, opts, "tray_status", nil)
		if res.IsError {
			t.Fatal(toolText(res))
		}
	}
	if n != 2 || len(hosts) != 2 || hosts[0] != "127.0.0.1:38080" || hosts[1] != "127.0.0.1:38081" {
		t.Fatalf("n=%d hosts=%#v", n, hosts)
	}
}

func TestSuppliedClientDoesNotFollowRedirect(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-one")
	var leaked []string
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		leaked = append(leaked, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer next.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		http.Redirect(w, r, next.URL+"/api/status", http.StatusFound)
	}))
	defer srv.Close()
	// A caller-supplied client follows redirects unless we replace CheckRedirect.
	opts := Options{
		StatusAddr:  "127.0.0.1:38080",
		trayBaseURL: srv.URL,
		GOOS:        "darwin",
		HTTPClient:  &http.Client{},
	}
	res := callTool(t, opts, "tray_status", nil)
	if !res.IsError {
		t.Fatal("redirect was treated as success")
	}
	if len(leaked) != 0 {
		t.Fatalf("token forwarded: %#v", leaked)
	}
}

func TestNoTrayOrIngestImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(path, "/internal/tray") || strings.HasSuffix(path, "/internal/ingest") {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

func TestStartIngestStructured(t *testing.T) {
	withTempTokenDir(t)
	writeToken(t, "token-one")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ignoreProbe(w, r) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"started":true,"cardPaths":["/mnt/card"]}`))
	}))
	defer srv.Close()
	opts := Options{StatusAddr: "127.0.0.1:38080", trayBaseURL: srv.URL, GOOS: "darwin"}
	res := callTool(t, opts, "start_ingest", map[string]any{"cardPath": "/mnt/card"})
	if res.IsError {
		t.Fatal(toolText(res))
	}
	got := structured(t, res)
	if got["started"] != true {
		t.Fatalf("structured = %#v", got)
	}
}

func withTempTokenDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_STATE_HOME", dir)
	}
}

func writeToken(t *testing.T, value string) string {
	t.Helper()
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}
	path, err := tokenFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return value
}

func tokenFile() (string, error) {
	dir, err := tokenDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sessiontoken.FileName), nil
}

func tokenDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "branchDAM", "logs"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs", "branchDAM"), nil
	default:
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".local", "state")
		}
		return filepath.Join(base, "branchdam-agent"), nil
	}
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// ignoreProbe drops the unsolicited GET / that sometimes lands on a fresh
// httptest listener. Tool calls only use /api/* with the pinned Host.
func ignoreProbe(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	http.NotFound(w, r)
	return true
}
