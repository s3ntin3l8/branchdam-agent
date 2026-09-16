package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:38080", true},
		{"127.5.6.7:38080", true},
		{"localhost:38080", true},
		{"[::1]:38080", true},
		{"0.0.0.0:38080", false},
		{"192.168.1.5:38080", false},
		{"example.com:38080", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopbackHost(tt.addr); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	const addr = "127.0.0.1:38080"
	tests := []struct {
		name     string
		origin   string
		secFetch string
		want     bool
	}{
		{"no headers", "", "", true},
		{"matching origin", "http://127.0.0.1:38080", "", true},
		{"mismatched origin", "http://evil.example", "", false},
		{"same-origin fetch metadata", "http://127.0.0.1:38080", "same-origin", true},
		{"cross-site fetch metadata always rejected", "http://127.0.0.1:38080", "cross-site", false},
		{"cross-site with no origin still rejected", "", "cross-site", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:38080/api/actions/drain", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.secFetch != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetch)
			}
			if got := originAllowed(req, addr); got != tt.want {
				t.Errorf("originAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenValid(t *testing.T) {
	tests := []struct {
		name        string
		serverToken string
		header      string
		want        bool
	}{
		{"correct token", "secret-token", "Bearer secret-token", true},
		{"wrong token", "secret-token", "Bearer nope", false},
		{"missing header", "secret-token", "", false},
		{"missing bearer prefix", "secret-token", "secret-token", false},
		{"empty server token fails closed", "", "Bearer anything", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &StatusServer{Token: tt.serverToken}
			req := httptest.NewRequest(http.MethodPost, "/api/actions/drain", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if got := srv.tokenValid(req); got != tt.want {
				t.Errorf("tokenValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegisterAPIRoutesSkipsNonLoopback(t *testing.T) {
	s := &StatusServer{Addr: "0.0.0.0:38080", Token: "tok"}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/status on a non-loopback bind = %d, want %d (route must not be registered at all)", rec.Code, http.StatusNotFound)
	}
}

func TestAPIRoutesRequireToken(t *testing.T) {
	s := &StatusServer{
		Addr:     "127.0.0.1:38080",
		Token:    "tok",
		Actions:  &spyActions{},
		Settings: &spySettings{},
	}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/settings"},
		{http.MethodPost, "/api/settings"},
		{http.MethodPost, "/api/settings/integration-path"},
		{http.MethodPost, "/api/settings/integration-rewrites"},
		{http.MethodPost, "/api/actions/ingest"},
		{http.MethodPost, "/api/actions/drain"},
		{http.MethodPost, "/api/actions/prune"},
		{http.MethodPost, "/api/actions/sync"},
		{http.MethodPost, "/api/actions/hook-install"},
		{http.MethodPost, "/api/actions/hook-reveal"},
		{http.MethodPost, "/api/actions/pause"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("without a token, %s %s = %d, want %d", rt.method, rt.path, rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestAPIRoutesRejectCrossSiteEvenWithValidToken(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: &spyActions{}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/actions/drain", nil)
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site request with a valid token = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// spyActions is a fake ActionRunner recording the last call made to each
// method, and returning configurable canned results.
type spyActions struct {
	ingestSummary IngestSummary
	drainSummary  DrainSummary
	drainRan      bool
	pruneSummary  PruneSummary
	pruneRan      bool
	syncSummary   SyncSummary
	syncRan       bool
	hookState     HookState
	hookRan       bool
	revealErr     error
	paused        bool

	lastCardPath string
	lastSyncID   IntegrationID
	lastHookID   HookID
	lastRevealID HookID
}

func (a *spyActions) TriggerIngest(_ context.Context, cardPath string) IngestSummary {
	a.lastCardPath = cardPath
	return a.ingestSummary
}
func (a *spyActions) TriggerDrain(_ context.Context) (DrainSummary, bool) {
	return a.drainSummary, a.drainRan
}
func (a *spyActions) TriggerPrune(_ context.Context) (PruneSummary, bool) {
	return a.pruneSummary, a.pruneRan
}
func (a *spyActions) TriggerSync(_ context.Context, id IntegrationID) (SyncSummary, bool) {
	a.lastSyncID = id
	return a.syncSummary, a.syncRan
}
func (a *spyActions) TriggerHookInstall(_ context.Context, id HookID) (HookState, bool) {
	a.lastHookID = id
	return a.hookState, a.hookRan
}
func (a *spyActions) RevealHook(id HookID) error {
	a.lastRevealID = id
	return a.revealErr
}
func (a *spyActions) Paused() bool     { return a.paused }
func (a *spyActions) SetPaused(v bool) { a.paused = v }

// spySettings is a fake Settings recording SetBool/SetInt/SetString/
// SetStringSlice/SetIntegrationPath/SetIntegrationRewrites calls and
// returning a configurable error from each.
type spySettings struct {
	snapshot SettingsView
	setErr   error

	lastBoolKey    string
	lastBoolVal    bool
	lastIntKey     string
	lastIntVal     int
	lastSliceKey   string
	lastSliceVal   []string
	lastStringKey  string
	lastStringVal  string
	lastPathID     IntegrationID
	lastPathVal    string
	lastRewriteID  IntegrationID
	lastRewriteVal string
}

func (s *spySettings) Snapshot() SettingsView { return s.snapshot }
func (s *spySettings) SetBool(key string, v bool) error {
	s.lastBoolKey, s.lastBoolVal = key, v
	return s.setErr
}
func (s *spySettings) SetInt(key string, v int) error {
	s.lastIntKey, s.lastIntVal = key, v
	return s.setErr
}
func (s *spySettings) SetStringSlice(key string, v []string) error {
	s.lastSliceKey, s.lastSliceVal = key, v
	return s.setErr
}
func (s *spySettings) SetString(key, v string) error {
	s.lastStringKey, s.lastStringVal = key, v
	return s.setErr
}
func (s *spySettings) PromptAndSet(_ SettingsField) (bool, error) { return false, nil }
func (s *spySettings) PromptAndSetIntegrationPath(_ IntegrationID) (bool, error) {
	return false, nil
}
func (s *spySettings) SetIntegrationPath(id IntegrationID, v string) error {
	s.lastPathID, s.lastPathVal = id, v
	return s.setErr
}
func (s *spySettings) PromptAndSetIntegrationRewrites(_ IntegrationID) (bool, error) {
	return false, nil
}
func (s *spySettings) SetIntegrationRewrites(id IntegrationID, v string) error {
	s.lastRewriteID, s.lastRewriteVal = id, v
	return s.setErr
}
func (s *spySettings) Reload() error             { return nil }
func (s *spySettings) OpenConfigFile() error     { return nil }
func (s *spySettings) RevealConfigFolder() error { return nil }

func newAuthedRequest(method, path string, body []byte) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	req.Header.Set("Authorization", "Bearer tok")
	return req
}

func TestHandleActionIngest(t *testing.T) {
	actions := &spyActions{ingestSummary: IngestSummary{CardPath: "/mnt/card", Submitted: 3}}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(ingestActionRequest{CardPath: "/mnt/card"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/ingest", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if actions.lastCardPath != "/mnt/card" {
		t.Errorf("TriggerIngest called with %q, want %q", actions.lastCardPath, "/mnt/card")
	}
	var got ingestActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Submitted != 3 {
		t.Errorf("Submitted = %d, want 3", got.Submitted)
	}
}

func TestHandleActionIngestRequiresCardPath(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: &spyActions{}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(ingestActionRequest{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/ingest", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleActionDrainReportsErr(t *testing.T) {
	actions := &spyActions{drainSummary: DrainSummary{Err: errors.New("boom")}, drainRan: true}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/drain", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got drainActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Ran || got.Err != "boom" {
		t.Errorf("got %+v, want Ran=true Err=boom", got)
	}
}

func TestHandleActionPruneNotRun(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: &spyActions{pruneRan: false}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/prune", nil))

	var got pruneActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Ran {
		t.Errorf("Ran = true, want false")
	}
}

func TestHandleActionSyncRequiresID(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: &spyActions{}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/sync", []byte(`{}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleActionSyncPassesID(t *testing.T) {
	actions := &spyActions{syncRan: true}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idActionRequest{ID: "resolve"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/sync", body))

	if actions.lastSyncID != IntegrationID("resolve") {
		t.Errorf("TriggerSync called with %q, want %q", actions.lastSyncID, "resolve")
	}
}

func TestHandleActionHookInstallPassesID(t *testing.T) {
	actions := &spyActions{hookState: HookState{Installed: true}}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idActionRequest{ID: "resolve"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/hook-install", body))

	if actions.lastHookID != HookID("resolve") {
		t.Errorf("TriggerHookInstall called with %q, want %q", actions.lastHookID, "resolve")
	}
	var got hookInstallActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Installed {
		t.Errorf("Installed = false, want true")
	}
}

func TestHandleActionHookRevealRequiresID(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: &spyActions{}}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/hook-reveal", []byte(`{}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleActionHookRevealPassesID(t *testing.T) {
	actions := &spyActions{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idActionRequest{ID: "resolve"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/hook-reveal", body))

	if actions.lastRevealID != HookID("resolve") {
		t.Errorf("RevealHook called with %q, want %q", actions.lastRevealID, "resolve")
	}
	var got hookRevealActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Err != "" {
		t.Errorf("Err = %q, want empty", got.Err)
	}
}

func TestHandleActionHookRevealReportsErr(t *testing.T) {
	actions := &spyActions{revealErr: errors.New("no Scripts folder found")}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idActionRequest{ID: "resolve"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/hook-reveal", body))

	var got hookRevealActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Err != "no Scripts folder found" {
		t.Errorf("Err = %q, want %q", got.Err, "no Scripts folder found")
	}
}

func TestHandleActionPauseSetsSessionGateOnly(t *testing.T) {
	actions := &spyActions{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: actions}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(pauseActionRequest{Paused: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/pause", body))

	if !actions.paused {
		t.Error("SetPaused was not called with true")
	}
	var got pauseActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Paused {
		t.Errorf("response Paused = false, want true")
	}
}

func TestActionRoutesReturn503WithoutActions(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok"}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/drain", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleAPISettingsGet(t *testing.T) {
	settings := &spySettings{snapshot: SettingsView{AgentID: "agent-1"}}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got SettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "agent-1")
	}
}

func TestHandleAPISettingsGetReturns503WithoutSettings(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok"}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/settings", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleAPISettingsPostBool(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "ingest.pauseUploadOnMetered", Value: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if settings.lastBoolKey != "ingest.pauseUploadOnMetered" || !settings.lastBoolVal {
		t.Errorf("SetBool called with (%q, %v)", settings.lastBoolKey, settings.lastBoolVal)
	}
}

func TestHandleAPISettingsPostInt(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "selfUpdate.checkIntervalHours", Value: 6})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if settings.lastIntKey != "selfUpdate.checkIntervalHours" || settings.lastIntVal != 6 {
		t.Errorf("SetInt called with (%q, %v)", settings.lastIntKey, settings.lastIntVal)
	}
}

func TestHandleAPISettingsPostRejectsFractionalInt(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "selfUpdate.checkIntervalHours", Value: 3.7})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if settings.lastIntKey != "" {
		t.Errorf("SetInt was called with a fractional value, want rejected before reaching Settings")
	}
}

func TestHandleAPISettingsPostRejectsOutOfRangeInt(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "selfUpdate.checkIntervalHours", Value: 1e18})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if settings.lastIntKey != "" {
		t.Errorf("SetInt was called with an out-of-range value, want rejected before reaching Settings")
	}
}

func TestHandleAPISettingsPostStringSlice(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "ingest.allowedExtensions", Value: []string{"cr3", "arw"}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(settings.lastSliceVal) != 2 || settings.lastSliceVal[0] != "cr3" {
		t.Errorf("SetStringSlice called with %v", settings.lastSliceVal)
	}
}

func TestHandleAPISettingsPostString(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "agentId", Value: "workstation-7"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if settings.lastStringKey != "agentId" || settings.lastStringVal != "workstation-7" {
		t.Errorf("SetString called with (%q, %q)", settings.lastStringKey, settings.lastStringVal)
	}
}

func TestHandleAPISettingsIntegrationPath(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idValueSettingsRequest{ID: string(IntegrationLuminar), Value: "/data/catalog.db"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings/integration-path", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if settings.lastPathID != IntegrationLuminar || settings.lastPathVal != "/data/catalog.db" {
		t.Errorf("SetIntegrationPath called with (%q, %q)", settings.lastPathID, settings.lastPathVal)
	}
}

func TestHandleAPISettingsIntegrationPathRequiresID(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idValueSettingsRequest{Value: "/data/catalog.db"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings/integration-path", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAPISettingsIntegrationRewrites(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idValueSettingsRequest{ID: string(IntegrationResolveDB), Value: `D:\Videos\:/storage/archive/videos/`})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings/integration-rewrites", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if settings.lastRewriteID != IntegrationResolveDB || settings.lastRewriteVal != `D:\Videos\:/storage/archive/videos/` {
		t.Errorf("SetIntegrationRewrites called with (%q, %q)", settings.lastRewriteID, settings.lastRewriteVal)
	}
}

func TestHandleAPISettingsIntegrationRewritesRequiresID(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(idValueSettingsRequest{Value: "a:b"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings/integration-rewrites", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAPISettingsPostRejectsUnsupportedType(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "some.key", Value: map[string]string{"a": "b"}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAPISettingsPostRequiresKey(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Value: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleAPISettingsPostPropagatesValidationError(t *testing.T) {
	settings := &spySettings{setErr: errors.New("invalid value")}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	body, _ := json.Marshal(settingsPatchRequest{Key: "some.key", Value: true})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/settings", body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
