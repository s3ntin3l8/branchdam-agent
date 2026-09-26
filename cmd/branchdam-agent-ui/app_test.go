//go:build windows || darwin

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
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

func TestSetPathMappingsPostsStructuredBody(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/settings/path-mappings" {
			t.Errorf("method/path = %s %s, want POST /api/settings/path-mappings", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	mappings := []PathMappingInput{{WorkstationPath: `D:\Photos\Archive`, ContainerPath: "/storage/archive"}}
	if _, err := a.SetPathMappings(mappings); err != nil {
		t.Fatalf("SetPathMappings: %v", err)
	}
	if !strings.Contains(string(gotBody), `"workstationPath":"D:\\Photos\\Archive"`) {
		t.Errorf("request body = %s, want workstationPath field", gotBody)
	}
	if !strings.Contains(string(gotBody), `"containerPath":"/storage/archive"`) {
		t.Errorf("request body = %s, want containerPath field", gotBody)
	}
}

// TestPathMappingFieldComparesReturnedAgainstPayload guards a self-review
// finding on this PR: renderPathMappingField's commit() must diff the
// server's response against the FILTERED payload it actually sent, not
// against the raw (possibly still-being-typed, unfiltered) entries array.
// Comparing against entries would read a normal in-progress row --
// filtered out of payload because one side is still blank -- as always
// "changed," triggering an unconditional render() that wipes the row the
// operator is mid-typing on every blur. There is no JS test harness in
// this repo (see TestIntegrationBlockWritesSyncTimeoutKey's own doc
// comment for why this source-grep is the only mechanical check
// available); a jsdom-based manual smoke test exercising the actual
// add/edit/save sequence is the real verification for this fix.
func TestPathMappingFieldComparesReturnedAgainstPayload(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "JSON.stringify(returned) !== JSON.stringify(payload)") {
		t.Error("renderPathMappingField's commit() must compare the server response against payload, not against entries -- see this test's own doc comment")
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

func TestTestConnectionPostsToActionRoute(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/test-connection" {
			t.Errorf("method/path = %s %s, want POST /api/actions/test-connection", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ran":true,"ok":true,"version":"1.10.0"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.TestConnection()
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !strings.Contains(got, `"version":"1.10.0"`) {
		t.Errorf("TestConnection() = %q, want the agent's raw response body passed through", got)
	}
}

func TestPairPostsToActionRoute(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/pair" {
			t.Errorf("method/path = %s %s, want POST /api/actions/pair", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"url":"branchdam://test"`) {
			t.Errorf("request body = %q, want url field", string(body))
		}
		_, _ = w.Write([]byte(`{"ok":true,"server":"http://example.com","agentId":"test-agent"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.Pair("branchdam://test")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if !strings.Contains(got, `"ok":true`) || !strings.Contains(got, `"server":"http://example.com"`) {
		t.Errorf("Pair() = %q, want the agent's raw response body passed through", got)
	}
}

func TestPairDeepLinkMarksSourceAndAcceptsPending(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"source":"deeplink"`) {
			t.Errorf("request body = %q, want source deeplink", string(body))
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true,"pending":true,"server":"http://example.com","agentId":"a"}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	got, err := newTestApp(t).PairDeepLink("branchdam://test")
	if err != nil {
		t.Fatalf("PairDeepLink: %v", err)
	}
	if !strings.Contains(got, `"pending":true`) {
		t.Errorf("PairDeepLink() = %q, want the 202 body passed through", got)
	}
}

func TestHandleOpenURLShowsDialogWhenTrayUnreachable(t *testing.T) {
	withTempAgentDir(t) // no session token: the tray isn't running
	var msg string
	orig := messageDialogFunc
	messageDialogFunc = func(_ context.Context, opts wailsruntime.MessageDialogOptions) (string, error) {
		msg = opts.Message
		return "", nil
	}
	t.Cleanup(func() { messageDialogFunc = orig })
	a := newTestApp(t)
	a.ctx = context.Background()

	a.HandleOpenURL("branchdam://?server=https%3A%2F%2Fdam.example.com&key=01234567890123456789012345678901&agent=dev-x")

	if !strings.Contains(msg, "start the tray app first") {
		t.Errorf("dialog message = %q, want a start-the-tray hint", msg)
	}
	if strings.Contains(msg, "01234567890123456789012345678901") {
		t.Error("dialog leaks the API key")
	}
}

func TestCheckForUpdatePostsToActionRoute(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/check-update" {
			t.Errorf("method/path = %s %s, want POST /api/actions/check-update", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ran":true,"status":{"UpdateFound":true,"LatestVersion":"1.12.0"}}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.CheckForUpdate()
	if err != nil {
		t.Fatalf("CheckForUpdate: %v", err)
	}
	if !strings.Contains(got, `"LatestVersion":"1.12.0"`) {
		t.Errorf("CheckForUpdate() = %q, want the agent's raw response body passed through", got)
	}
}

func TestApplyUpdatePostsToActionRoute(t *testing.T) {
	withTempAgentDir(t)
	if _, err := sessiontoken.Generate(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/actions/apply-update" {
			t.Errorf("method/path = %s %s, want POST /api/actions/apply-update", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"started":true,"status":{"Phase":"downloading"}}`))
	}))
	defer srv.Close()
	withStatusServerAddr(t, strings.TrimPrefix(srv.URL, "http://"))

	a := newTestApp(t)
	got, err := a.ApplyUpdate()
	if err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if !strings.Contains(got, `"started":true`) {
		t.Errorf("ApplyUpdate() = %q, want started=true response", got)
	}
}

func TestConfirmApplyUpdateUsesNativeQuestionDialog(t *testing.T) {
	orig := messageDialogFunc
	t.Cleanup(func() { messageDialogFunc = orig })
	var got wailsruntime.MessageDialogOptions
	messageDialogFunc = func(_ context.Context, options wailsruntime.MessageDialogOptions) (string, error) {
		got = options
		return "Install and restart", nil
	}

	a := newTestApp(t)
	confirmed, err := a.ConfirmApplyUpdate("1.14.1")
	if err != nil {
		t.Fatalf("ConfirmApplyUpdate: %v", err)
	}
	if !confirmed {
		t.Fatal("confirmed = false, want true for the install button")
	}
	if got.Type != wailsruntime.QuestionDialog || got.Title != "Confirm install and restart" {
		t.Errorf("dialog type/title = %q/%q, want question/confirm title", got.Type, got.Title)
	}
	if got.Message == "" || !strings.Contains(got.Message, "1.14.1") {
		t.Errorf("dialog message = %q, want release version", got.Message)
	}
	if !strings.Contains(strings.Join(got.Buttons, ","), "Install and restart") {
		t.Errorf("dialog buttons = %v, want install action", got.Buttons)
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

// TestIntegrationBlockRendersFriendlyTitle guards issue #221: the Settings
// form's per-integration heading must render iv.Title ("Luminar Neo",
// "DaVinci Resolve"), falling back to the raw iv.ID only when Title is
// empty -- the same iv.Title || iv.ID pattern renderIntegrations/
// renderHooks already use for the live-status side of this same window.
// tray.IntegrationView carries no json: struct tags (see that type's own
// doc comment), so this is the only guard on the fallback itself: a Go
// test asserting the Go struct field is non-empty would prove nothing
// about what the JS actually does with it.
func TestIntegrationBlockRendersFriendlyTitle(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, "h3.textContent = iv.Title || iv.ID;") {
		t.Error("frontend/dist/app.js's renderIntegrationBlock no longer renders iv.Title || iv.ID for its <h3> heading")
	}
}

// TestIntegrationBlockHidesDetailWhenDisabled guards the enabled-gating
// added on this PR: Dry run/Catalog path/Path rewrites/Sync interval/Sync
// timeout are all noise for an integration that isn't running, so they're
// wrapped in one .integration-detail element toggled via `hidden` rather
// than rebuilt -- see that element's own doc comment in app.js for why a
// rebuild-in-place would be wrong (it would have to re-derive visibility
// from the checkbox's POST-revert state, which onSaved already handles in
// exactly one place). There is no JS test harness in this repo (see
// TestIntegrationBlockWritesSyncTimeoutKey's own doc comment for why this
// source-grep is the only mechanical check available); a jsdom-based
// manual smoke test exercising the actual enable/disable/reject sequence
// is the real verification for this behavior.
func TestIntegrationBlockHidesDetailWhenDisabled(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if !strings.Contains(body, `detail.className = "integration-detail"`) {
		t.Error("frontend/dist/app.js's renderIntegrationBlock no longer wraps its detail fields in an .integration-detail element")
	}
	if !strings.Contains(body, "detail.hidden = !iv.Enabled;") {
		t.Error("frontend/dist/app.js's renderIntegrationBlock no longer hides .integration-detail for a disabled integration at initial render")
	}
	if !strings.Contains(body, "detail.hidden = !checked;") {
		t.Error("frontend/dist/app.js's Enabled checkbox no longer toggles .integration-detail's visibility on save")
	}
}

// TestCategoryPanelsGroupSettingsWithTheirStatus is the nav-pane-era
// successor to the tray/window UX rethink's original
// TestSettingsSectionsPrecedeLiveSections: that test's premise -- one global
// "every config section before every live-status section" ordering -- died
// the moment settings and status paired up per category (the operator's own
// request: "setting and status should go together for their corresponding
// sections"). The underlying concern it existed for survives in a form that
// fits the new structure instead: within EACH panel, that panel's own
// settings subsection must still precede its own live-status subsection(s),
// so configuring a category is always the first thing an operator sees
// inside it, never buried below status that can't populate until
// configuration is done.
func TestCategoryPanelsGroupSettingsWithTheirStatus(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	categories := []struct {
		panel  string
		config string
		live   []string
	}{
		{"panel-server", "settings-server-config", []string{"server-body"}},
		{"panel-storage", "settings-storage-config", []string{"ingest-body", "queue-body", "watch-body"}},
		{"panel-integrations", "settings-integrations-config", []string{"integrations-body", "hooks-body"}},
		{"panel-behavior", "settings-behavior-config", nil},
		{"panel-selfupdate", "settings-selfupdate-config", []string{"selfupdate-body"}},
	}

	mainEnd := strings.Index(body, "</main>")
	if mainEnd == -1 {
		t.Fatal(`index.html missing "</main>" -- can't bound the last panel's span`)
	}

	// uniqueIndex is strings.Index plus a uniqueness check: a bare
	// strings.Index only ever finds the FIRST occurrence, silently matching
	// what getElementById itself would resolve to if an id were
	// accidentally duplicated elsewhere in the file -- the same
	// silent-wrong-behavior class every other test in this file exists to
	// catch, not a hardening this one gets to skip (Hermes review finding
	// on this PR).
	uniqueIndex := func(id string) int {
		needle := `id="` + id + `"`
		if n := strings.Count(body, needle); n != 1 {
			t.Fatalf("index.html has %d occurrences of %s, want exactly 1", n, needle)
		}
		return strings.Index(body, needle)
	}

	for i, c := range categories {
		start := uniqueIndex(c.panel)
		end := mainEnd
		if i+1 < len(categories) {
			end = uniqueIndex(categories[i+1].panel)
		}
		if start >= end {
			t.Fatalf("panel %q's span is empty or inverted (start=%d, end=%d)", c.panel, start, end)
		}

		configIdx := uniqueIndex(c.config)
		if configIdx < start || configIdx >= end {
			t.Errorf("config container %q (byte %d) is outside panel %q's span [%d, %d)", c.config, configIdx, c.panel, start, end)
		}

		for _, liveID := range c.live {
			liveIdx := uniqueIndex(liveID)
			if liveIdx < start || liveIdx >= end {
				t.Errorf("live-status container %q (byte %d) is outside panel %q's span [%d, %d)", liveID, liveIdx, c.panel, start, end)
			}
			if liveIdx < configIdx {
				t.Errorf("live-status container %q (byte %d) precedes its own panel's config container %q (byte %d) -- settings must come first within a panel", liveID, liveIdx, c.config, configIdx)
			}
		}
	}
}

// TestNavItemsAndPanelsCorrespond guards the nav/panel wiring itself: every
// nav button's data-panel must name a panel that actually exists, and every
// panel must be reachable from some nav button -- either failure mode is
// silent in the running window (a dead click, or permanently hidden
// content), so this is checked mechanically instead. Depends on the same
// attribute-order convention index.html documents: "class" before
// "data-panel" on nav buttons, "class" before "id" on panels.
func TestNavItemsAndPanelsCorrespond(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	navRe := regexp.MustCompile(`class="nav-item[^"]*"\s+data-panel="([^"]+)"`)
	panelRe := regexp.MustCompile(`class="panel[^"]*"\s+id="([^"]+)"`)

	navIDs := map[string]int{}
	for _, m := range navRe.FindAllStringSubmatch(body, -1) {
		navIDs[m[1]]++
	}
	panelIDs := map[string]int{}
	for _, m := range panelRe.FindAllStringSubmatch(body, -1) {
		panelIDs[m[1]]++
	}

	if len(navIDs) == 0 {
		t.Fatal("no nav buttons found -- regex or markup drifted")
	}
	if len(panelIDs) == 0 {
		t.Fatal("no panels found -- regex or markup drifted")
	}

	for id, n := range navIDs {
		if n > 1 {
			t.Errorf("nav button targeting %q appears %d times, want 1", id, n)
		}
		if panelIDs[id] == 0 {
			t.Errorf("nav button targets %q, but no panel with that id exists", id)
		}
	}
	for id, n := range panelIDs {
		if n > 1 {
			t.Errorf("panel %q appears %d times, want 1", id, n)
		}
		if navIDs[id] == 0 {
			t.Errorf("panel %q exists but no nav button targets it -- unreachable content", id)
		}
	}
}

// TestExactlyOneDefaultPanel pins the operator's own decision: the window
// always opens on the Server panel, with no persistence of the
// last-viewed category across reopens (simpler, and keeps steering a
// first-run operator back to setup rather than wherever they last
// clicked). Exactly one panel/nav-item may start "active", and it must be
// panel-server -- two active panels would show overlapping content, zero
// would show a blank content pane, and the wrong one would silently
// contradict this decision on every window open.
func TestExactlyOneDefaultPanel(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	if n := strings.Count(body, `class="panel active"`); n != 1 {
		t.Errorf(`got %d occurrences of class="panel active", want exactly 1`, n)
	}
	if n := strings.Count(body, `class="nav-item active"`); n != 1 {
		t.Errorf(`got %d occurrences of class="nav-item active", want exactly 1`, n)
	}

	activePanelRe := regexp.MustCompile(`class="panel active"\s+id="([^"]+)"`)
	m := activePanelRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal(`no panel matches class="panel active" id="..."`)
	}
	if m[1] != "panel-server" {
		t.Errorf("default active panel is %q, want %q", m[1], "panel-server")
	}

	activeNavRe := regexp.MustCompile(`class="nav-item active"\s+data-panel="([^"]+)"`)
	nm := activeNavRe.FindStringSubmatch(body)
	if nm == nil {
		t.Fatal(`no nav button matches class="nav-item active" data-panel="..."`)
	}
	if nm[1] != m[1] {
		t.Errorf("active nav button targets %q, but active panel is %q -- they must agree", nm[1], m[1])
	}
}

// TestCategorySwitchingNeverRerenders is showCategory's own sibling to
// TestStatusPollNeverRebuildsSettingsContainers: switching categories must
// only toggle CSS visibility, never re-render a panel's contents. If
// showCategory (or initNav) ever called renderSettingsForm,
// renderIntegrationBlock, or rebuilt a container via innerHTML, an
// operator's mid-typing settings edit could be silently wiped out by
// clicking a nav item -- the same failure class the poll-clobber test
// exists to catch, reintroduced through a second door. Mirrors that test's
// own extraction mechanism exactly, including failing loudly (not
// vacuously) if either anchor goes missing.
func TestCategorySwitchingNeverRerenders(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	start := strings.Index(body, "function showCategory(")
	if start == -1 {
		t.Fatal("app.js: could not find `function showCategory(`")
	}
	end := strings.Index(body[start:], "\nfunction initNav(")
	if end == -1 {
		t.Fatal("app.js: could not find the end of showCategory (expected `function initNav(` to follow it)")
	}
	navBody := body[start : start+end]

	for _, bad := range []string{"-config", "renderSettingsForm", "renderIntegrationBlock", "innerHTML"} {
		if strings.Contains(navBody, bad) {
			t.Errorf("showCategory must never reference %q -- category switching must only toggle CSS visibility, never re-render a panel", bad)
		}
	}
}

// TestInactivePanelsAreHiddenByCSS guards the one CSS rule the whole nav
// depends on: without it, every panel renders simultaneously (back to the
// original one-long-page layout) and clicking a nav item does nothing
// visible -- a silent no-op, not an error, which is why this is pinned
// mechanically rather than left to a human noticing.
func TestInactivePanelsAreHiddenByCSS(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/style.css")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Join(strings.Fields(string(src)), "")

	if !strings.Contains(body, ".panel{display:none") {
		t.Error(`style.css missing ".panel { display: none }" -- panels are not hidden by default`)
	}
	if !strings.Contains(body, ".panel.active{display:") {
		t.Error(`style.css missing ".panel.active { display: ... }" -- the active panel has no override to become visible`)
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
	if !strings.Contains(renderBody, "renderSetupBanner(status)") {
		t.Error("render(view) must call renderSetupBanner(status) so the missing-fields banner refreshes on every status poll")
	}
}

// TestRenderIntegrationsConsultsEnabledByID guards the live-status half of
// this PR's enabled-gating: a disabled integration's row must not appear
// under Integrations status either, not just in its own settings detail
// block. enabledByID is a module-level Map, not part of the once-only
// settings snapshot, precisely so render(view) -- gated by
// TestStatusPollNeverRebuildsSettingsContainers above against ever
// touching a "-config" container or calling renderSettingsForm -- can
// still consult it without violating that separation.
func TestRenderIntegrationsConsultsEnabledByID(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	start := strings.Index(body, "function renderIntegrations(status) {")
	if start == -1 {
		t.Fatal("app.js: could not find `function renderIntegrations(status) {`")
	}
	end := strings.Index(body[start:], "\nfunction ")
	if end == -1 {
		t.Fatal("app.js: could not find the end of renderIntegrations")
	}
	fnBody := body[start : start+end]

	if !strings.Contains(fnBody, "enabledByID.get(i.ID) !== false") {
		t.Error("renderIntegrations no longer filters out a disabled integration's live-status row")
	}
	if strings.Contains(fnBody, "-config") {
		t.Error("renderIntegrations must never reference a \"-config\" container -- it runs inside render(view)'s poll-only call graph")
	}
}

// TestSetupBannerContainerIsNotASettingsContainer pins the id convention
// TestStatusPollNeverRebuildsSettingsContainers enforces on app.js's own
// render() body: #setup-banner is rebuilt by every 5s poll (it renders
// Status.missingFields, which can change the moment an operator fixes a
// field), so its id must never end in "-config" -- that suffix is reserved
// for the five containers loadSettings() owns exclusively.
func TestSetupBannerContainerIsNotASettingsContainer(t *testing.T) {
	src, err := os.ReadFile("frontend/dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `id="setup-banner"`) {
		t.Fatal("index.html missing #setup-banner")
	}
	if strings.Contains(body, `id="setup-banner-config"`) {
		t.Error("#setup-banner must not be a \"-config\" container -- it is rebuilt on every status poll, not loaded once by loadSettings()")
	}
}
