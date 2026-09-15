//go:build windows || darwin

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

// withTempAgentDir points internal/agentlog (and therefore
// internal/sessiontoken) at a temp directory, mirroring the seam
// sessiontoken's own tests use for the "other" GOOS branch -- windows and
// darwin each need their own env var, since agentlog.pathForGOOS switches
// on runtime.GOOS rather than honoring XDG_STATE_HOME on either.
func withTempAgentDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	}
}

func withStatusServerAddr(t *testing.T, addr string) {
	t.Helper()
	orig := statusServerAddrFunc
	statusServerAddrFunc = func() (string, error) { return addr, nil }
	t.Cleanup(func() { statusServerAddrFunc = orig })
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	a := NewApp()
	a.Startup(context.Background())
	t.Cleanup(func() { a.ctx = nil })
	return a
}

func TestStatusJSONReturnsAgentBodyOnSuccess(t *testing.T) {
	withTempAgentDir(t)
	token, err := sessiontoken.Generate()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			t.Errorf("path = %q, want /api/status", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer %s", got, token)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.2.3"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.StatusJSON()
	if err != nil {
		t.Fatalf("StatusJSON: %v", err)
	}
	if got != `{"version":"1.2.3"}` {
		t.Errorf("StatusJSON() = %q", got)
	}
}

func TestStatusJSONWithoutRunningAgent(t *testing.T) {
	withTempAgentDir(t) // no sessiontoken.Generate call -- no token file exists

	a := newTestApp(t)
	_, err := a.StatusJSON()
	if err == nil {
		t.Fatal("StatusJSON succeeded with no session token present, want an error")
	}
}

func TestStatusJSONUnreachableAgent(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1:1 -- a port nothing listens on, so the request fails at
	// the transport level rather than getting any HTTP response at all.
	withStatusServerAddr(t, "127.0.0.1:1")

	a := newTestApp(t)
	_, err := a.StatusJSON()
	if err == nil {
		t.Fatal("StatusJSON succeeded against an unreachable address, want an error")
	}
}

func TestStatusJSONStaleToken(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	_, err := a.StatusJSON()
	if err == nil {
		t.Fatal("StatusJSON succeeded against a 401 response, want an error")
	}
}

func TestSettingsJSONReturnsAgentBodyOnSuccess(t *testing.T) {
	withTempAgentDir(t)
	token, err := sessiontoken.Generate()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/settings" {
			t.Errorf("method/path = %s %s, want GET /api/settings", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization header = %q, want Bearer %s", got, token)
		}
		_, _ = w.Write([]byte(`{"agentId":"box"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.SettingsJSON()
	if err != nil {
		t.Fatalf("SettingsJSON: %v", err)
	}
	if got != `{"agentId":"box"}` {
		t.Errorf("SettingsJSON() = %q", got)
	}
}

func TestSetSettingPostsKeyValueAndReturnsBody(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/settings" {
			t.Errorf("method/path = %s %s, want POST /api/settings", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"agentId":"workstation-7"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.SetSetting("agentId", "workstation-7")
	if err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got != `{"agentId":"workstation-7"}` {
		t.Errorf("SetSetting() = %q", got)
	}
	if !strings.Contains(string(gotBody), `"key":"agentId"`) || !strings.Contains(string(gotBody), `"value":"workstation-7"`) {
		t.Errorf("request body = %s, want key/value pair", gotBody)
	}
}

func TestSetSettingSurfacesAgentErrorBody(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "agentId must not be empty", http.StatusBadRequest)
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	_, err := a.SetSetting("agentId", "   ")
	if err == nil {
		t.Fatal("SetSetting succeeded against a 400 response, want an error")
	}
	if !strings.Contains(err.Error(), "agentId must not be empty") {
		t.Errorf("error = %q, want it to surface the agent's rejection reason", err)
	}
}

func TestSetIntegrationPathPostsIDAndValue(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/settings/integration-path" {
			t.Errorf("method/path = %s %s, want POST /api/settings/integration-path", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	if _, err := a.SetIntegrationPath("luminar", "/data/catalog.db"); err != nil {
		t.Fatalf("SetIntegrationPath: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"luminar"`) || !strings.Contains(string(gotBody), `"value":"/data/catalog.db"`) {
		t.Errorf("request body = %s, want id/value pair", gotBody)
	}
}

func TestSetIntegrationRewritesPostsIDAndValue(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/settings/integration-rewrites" {
			t.Errorf("method/path = %s %s, want POST /api/settings/integration-rewrites", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	if _, err := a.SetIntegrationRewrites("resolveDb", "D:\\Videos\\:/storage/archive/videos/"); err != nil {
		t.Fatalf("SetIntegrationRewrites: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"resolveDb"`) {
		t.Errorf("request body = %s, want id field", gotBody)
	}
}

func TestPickDirectoryBeforeStartupErrors(t *testing.T) {
	a := NewApp() // Startup never called -- a.ctx stays nil
	if _, err := a.PickDirectory("Pick a folder"); err == nil {
		t.Fatal("PickDirectory succeeded with a nil ctx, want an error")
	}
}

func TestPickFileBeforeStartupErrors(t *testing.T) {
	a := NewApp() // Startup never called -- a.ctx stays nil
	if _, err := a.PickFile("Pick a file", []string{"*.json"}); err == nil {
		t.Fatal("PickFile succeeded with a nil ctx, want an error")
	}
}

func TestStatusJSONRejectsInvalidJSONBody(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	_, err := a.StatusJSON()
	if err == nil {
		t.Fatal("StatusJSON succeeded with a non-JSON 200 body, want an error")
	}
}
