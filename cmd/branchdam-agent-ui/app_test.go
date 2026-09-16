//go:build windows || darwin

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestVersionReturnsStampedValue(t *testing.T) {
	orig := version
	version = "1.2.3"
	t.Cleanup(func() { version = orig })

	a := NewApp()
	if got := a.Version(); got != "1.2.3" {
		t.Errorf("Version() = %q, want %q", got, "1.2.3")
	}
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

// TestSetSettingPostsNumericValue pins the exact wire path app.js's two
// <select> fields (self-update interval, integration sync interval) use:
// JS Number() -> a Go `any` parameter -> encoding/json's float64 default
// -> re-marshaled into the request body. A regression here (e.g. losing
// precision, or the value round-tripping as a string) would only surface
// on real hardware -- see Hermes review finding on PR #210.
func TestSetSettingPostsNumericValue(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	if _, err := a.SetSetting("selfUpdate.checkIntervalHours", float64(24)); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if !strings.Contains(string(gotBody), `"key":"selfUpdate.checkIntervalHours"`) || !strings.Contains(string(gotBody), `"value":24`) {
		t.Errorf("request body = %s, want a bare numeric value (24), not a quoted string", gotBody)
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

func TestTriggerSyncPostsID(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/sync" {
			t.Errorf("method/path = %s %s, want POST /api/actions/sync", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"Ran":true,"Emitted":3}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.TriggerSync("luminar")
	if err != nil {
		t.Fatalf("TriggerSync: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"luminar"`) {
		t.Errorf("request body = %s, want id field", gotBody)
	}
	if !strings.Contains(got, `"Emitted":3`) {
		t.Errorf("TriggerSync() = %q, want the agent's raw response body passed through", got)
	}
}

func TestTriggerHookInstallPostsID(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/hook-install" {
			t.Errorf("method/path = %s %s, want POST /api/actions/hook-install", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"Installed":true}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.TriggerHookInstall("resolve")
	if err != nil {
		t.Fatalf("TriggerHookInstall: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"resolve"`) {
		t.Errorf("request body = %s, want id field", gotBody)
	}
	if !strings.Contains(got, `"Installed":true`) {
		t.Errorf("TriggerHookInstall() = %q, want the agent's raw response body passed through", got)
	}
}

func TestRevealHookPostsIDAndReturnsNilOnSuccess(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/hook-reveal" {
			t.Errorf("method/path = %s %s, want POST /api/actions/hook-reveal", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	if err := a.RevealHook("resolve"); err != nil {
		t.Fatalf("RevealHook: %v", err)
	}
	if !strings.Contains(string(gotBody), `"id":"resolve"`) {
		t.Errorf("request body = %s, want id field", gotBody)
	}
}

func TestRevealHookSurfacesAgentErrField(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"err":"no Scripts folder found"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	err := a.RevealHook("resolve")
	if err == nil || !strings.Contains(err.Error(), "no Scripts folder found") {
		t.Errorf("RevealHook() error = %v, want it to surface the agent's err field", err)
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

// TestIntegrationBlockWritesSyncTimeoutKey guards frontend/dist/app.js's
// Sync timeout field, the one config value the tray's own per-integration
// submenu (internal/tray/integrationsmenu.go) used to own exclusively and
// that this window's renderIntegrationBlock had to gain before that
// submenu could be deleted without losing functionality.
//
// There is no JS test harness in this repo -- no package.json, no
// bundler -- and frontend/dist/app.js is a committed artifact, not
// generated at build time. This Go-reads-a-non-Go-file guard is the only
// mechanical check available, the same precedent
// internal/selfupdate/release_workflow_contract_test.go sets by parsing
// release-binaries.yml directly rather than trusting a comment. It only
// runs on this package's own windows/darwin-tagged test jobs, never on
// the Linux required check.
func TestIntegrationBlockWritesSyncTimeoutKey(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "Sync timeout") {
		t.Error(`frontend/dist/app.js no longer renders a "Sync timeout" field`)
	}
	// Must match the exact source form the adjacent "Sync every" field
	// uses (a JS template literal), not string concatenation -- both must
	// write to the identical config.yaml key
	// internal/tray/integrationsmenu.go's integrationKey and
	// cmd/branchdam-agent/integrations.go's IntegrationBuilder.ConfigKey
	// independently derive on the agent side.
	if !strings.Contains(body, "`integrations.${iv.ID}.timeoutSecs`") {
		t.Error("frontend/dist/app.js's Sync timeout field doesn't write the `integrations.${iv.ID}.timeoutSecs` key")
	}
	// The UX rethink moved renderIntegrationBlock's loop into the
	// "Integration settings" config section (index.html's own <h2> now
	// supplies that heading) and deleted the bare <h3>Integrations</h3>
	// renderSettingsForm used to prepend -- two identically-worded
	// headings one below the other would have been worse than the
	// duplicate-tray-menu bug this whole rethink started from.
	if strings.Contains(body, `h3.textContent = "Integrations"`) {
		t.Error("frontend/dist/app.js still renders a bare <h3>Integrations</h3> above the integration blocks -- the section <h2> now supplies that heading")
	}
}

// TestSettingsSectionsPrecedeLiveSections pins decision 2 of the tray/window
// UX rethink ("settings order also confusing, integration status before
// setup"): every config-zone section (id ending "-config") must appear in
// index.html before every live-status section (id ending "-body"), so
// first-run configuration is the first thing an operator sees, not buried
// below status that can't populate until configuration is done.
func TestSettingsSectionsPrecedeLiveSections(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	configIDs := []string{
		"settings-server-config",
		"settings-storage-config",
		"settings-behavior-config",
		"settings-selfupdate-config",
		"settings-integrations-config",
	}
	liveIDs := []string{
		"server-body",
		"ingest-body",
		"queue-body",
		"watch-body",
		"integrations-body",
		"hooks-body",
		"selfupdate-body",
	}

	lastConfigIdx := -1
	for _, id := range configIDs {
		idx := strings.Index(body, `id="`+id+`"`)
		if idx == -1 {
			t.Fatalf("index.html missing config container %q", id)
		}
		if idx > lastConfigIdx {
			lastConfigIdx = idx
		}
	}
	for _, id := range liveIDs {
		idx := strings.Index(body, `id="`+id+`"`)
		if idx == -1 {
			t.Fatalf("index.html missing live-status container %q", id)
		}
		if idx < lastConfigIdx {
			t.Errorf("live-status container %q (byte %d) appears before the last config container (byte %d) -- settings must come first", id, idx, lastConfigIdx)
		}
	}
}

// TestStatusPollNeverRebuildsSettingsContainers is the load-bearing test for
// the config-zone/live-zone split: render(view) runs on every 5s status
// poll and rebuilds each live-status container's DOM via innerHTML. If it
// were ever changed to also touch a "-config" container (or to call
// renderSettingsForm), an operator mid-typing into a settings field would
// have their edit silently wiped out on the next poll tick -- a failure
// mode with no error, no crash, just lost input. This mechanically pins
// that render()'s own function body never mentions a "-config" id or
// renderSettingsForm; loadSettings() -- which does both -- is called
// exactly once, outside render()'s call graph, and must stay that way.
func TestStatusPollNeverRebuildsSettingsContainers(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	start := strings.Index(body, "function render(view) {")
	if start == -1 {
		t.Fatal("app.js: could not find `function render(view) {`")
	}
	end := strings.Index(body[start:], "\nfunction showError(")
	if end == -1 {
		t.Fatal("app.js: could not find the end of render(view) (expected `function showError(` to follow it)")
	}
	renderBody := body[start : start+end]

	if strings.Contains(renderBody, "-config") {
		t.Error("render(view) must never reference a \"-config\" container -- that's the settings poll-clobber bug this test exists to catch")
	}
	if strings.Contains(renderBody, "renderSettingsForm") {
		t.Error("render(view) must never call renderSettingsForm -- settings load exactly once, via loadSettings(), never on the status poll")
	}
}
