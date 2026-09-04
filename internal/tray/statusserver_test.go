package tray

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

func TestNormalizeLoopbackRewritesBarePort(t *testing.T) {
	cases := map[string]string{
		":38080":            "127.0.0.1:38080",
		"":                  "127.0.0.1:38080",
		"127.0.0.1:9000":    "127.0.0.1:9000",
		"0.0.0.0:9000":      "0.0.0.0:9000", // an explicit choice is left alone
		"localhost:9000":    "localhost:9000",
		"192.168.1.5:38080": "192.168.1.5:38080",
	}
	for in, want := range cases {
		if got := normalizeLoopback(in); got != want {
			t.Errorf("normalizeLoopback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewStatusServerNormalizesAddr(t *testing.T) {
	s := NewStatusServer(":1234", func() Status { return Status{} }, func() SettingsView { return SettingsView{} }, "1.2.3")
	if s.Addr != "127.0.0.1:1234" {
		t.Errorf("got Addr=%q, want a loopback-only rewrite of a bare port", s.Addr)
	}
	if s.StatusURL() != "http://127.0.0.1:1234/" {
		t.Errorf("got StatusURL()=%q", s.StatusURL())
	}
}

func TestHandleIndexRendersStatus(t *testing.T) {
	s := &StatusServer{
		Addr:    "127.0.0.1:0",
		Version: "9.9.9",
		StatusFunc: func() Status {
			return Status{
				WatchDirs:   []string{"/media/card1"},
				ScratchNote: "/local/scratch (usage tracking not yet implemented)",
				QueueStatus: QueueStatus{Configured: true, Counts: QueueCounts{AwaitingUpload: 2, Failed: 1}},
				LastIngest: &IngestSummary{
					CardPath:  "/media/card1",
					StartedAt: time.Now(),
					Submitted: 3,
					Skipped:   1,
				},
				SelfUpdate: UpdateStatus{Enabled: false},
			}
		},
	}

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"9.9.9", "/media/card1", "disabled", "3 submitted", "2 pending", "1 permanently failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q\n---\n%s", want, body)
		}
	}
	// The status page must never render the server API key -- it's a
	// localhost page but still should not become a place secrets leak to.
	if strings.Contains(body, "apiKey") {
		t.Error("response body must never mention apiKey")
	}
}

// TestHandleIndexNeverFabricatesQueueNumbers is the Phase 3c regression
// test for the invariant QueueStatusStub used to hold as a literal string:
// an unconfigured or unreadable queue must never render as "0 pending".
func TestHandleIndexNeverFabricatesQueueNumbers(t *testing.T) {
	cases := []struct {
		name string
		st   Status
		want []string
		bad  []string
	}{
		{
			name: "not configured",
			st:   Status{QueueStatus: QueueStatus{Configured: false}},
			want: []string{"not configured"},
			bad:  []string{"0 pending"},
		},
		{
			name: "read error",
			st:   Status{QueueStatus: QueueStatus{Configured: true, Err: errors.New("queue.db: disk I/O error")}},
			want: []string{"disk I/O error"},
			bad:  []string{"0 pending"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StatusServer{StatusFunc: func() Status { return tc.st }}
			req := httptest.NewRequest("GET", "/", nil)
			rec := httptest.NewRecorder()
			s.handleIndex(rec, req)
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("response body missing %q\n---\n%s", want, body)
				}
			}
			for _, bad := range tc.bad {
				if strings.Contains(body, bad) {
					t.Errorf("response body must never fabricate %q\n---\n%s", bad, body)
				}
			}
		})
	}
}

func TestListenIsTheSingleInstanceGuard(t *testing.T) {
	s1 := NewStatusServer("127.0.0.1:0", func() Status { return Status{} }, func() SettingsView { return SettingsView{} }, "1.0.0")
	ln1, err := s1.Listen()
	if err != nil {
		t.Fatalf("first Listen() failed: %v", err)
	}
	defer func() { _ = ln1.Close() }()

	s2 := NewStatusServer(ln1.Addr().String(), func() Status { return Status{} }, func() SettingsView { return SettingsView{} }, "1.0.0")
	if _, err := s2.Listen(); err == nil {
		t.Error("expected a second Listen() on the same address to fail -- this is the single-instance guard a self-update relaunch relies on")
	}
}

// TestHandleIndexIntegrationsNoIntegrationsRegistered covers the empty
// case: Status.Integrations is nil (e.g. cmd/branchdam-agent wired no
// syncers at all).
func TestHandleIndexIntegrationsNoIntegrationsRegistered(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{} }}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "No catalog integrations registered") {
		t.Errorf("response body missing the no-integrations message\n---\n%s", body)
	}
}

// TestHandleIndexIntegrationsUnregisteredRendersNotConfigured covers an
// entry present in the registry (so it appears in Status.Integrations) but
// with Registered=false -- e.g. enabled with no catalogPath yet, or simply
// never wired -- which must render as "not configured," never as a
// fabricated enabled/disabled or dry-run state.
func TestHandleIndexIntegrationsUnregisteredRendersNotConfigured(t *testing.T) {
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{Integrations: []IntegrationStatus{{ID: IntegrationLuminar, Registered: false}}}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "not configured") {
		t.Errorf("response body missing 'not configured'\n---\n%s", body)
	}
	// "enabled" alone isn't checked here -- it's a substring of the
	// unrelated "selfUpdate.enabled: false in config" self-update note.
	if strings.Contains(body, "dry run") || strings.Contains(body, "live") || strings.Contains(body, "catalog") {
		t.Errorf("an unregistered integration must not render a fabricated enabled/dry-run/catalog state\n---\n%s", body)
	}
}

// TestHandleIndexIntegrationsDryRunLastSyncShowsMarker pins the issue #61
// acceptance criterion verbatim: the "(dry run -- nothing was emitted)"
// marker must be driven by LastSync.DryRun (what the PASS actually did),
// not by the config's current dryRun checkbox -- those can disagree, e.g.
// an operator unticks dry run right after a dry-run pass completed. Emitted
// is otherwise indistinguishable from a real emit count, so the marker is
// the only thing keeping it from being actively misleading.
func TestHandleIndexIntegrationsDryRunLastSyncShowsMarker(t *testing.T) {
	at := time.Now().Add(-2 * time.Minute)
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{Integrations: []IntegrationStatus{{
				ID:         IntegrationLuminar,
				Registered: true,
				LastSync:   &SyncSummary{At: at, DryRun: true, PairsFound: 5, Emitted: 12, Skipped: 1},
			}}}
		},
		SettingsFunc: func() SettingsView {
			return SettingsView{Integrations: []IntegrationView{{ID: IntegrationLuminar, Enabled: true, DryRun: false}}}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "12 emitted") {
		t.Errorf("response body missing the emitted count\n---\n%s", body)
	}
	if !strings.Contains(body, "dry run") || !strings.Contains(body, "nothing was emitted") {
		t.Errorf("response body missing the dry-run marker even though LastSync.DryRun=true (config DryRun=false)\n---\n%s", body)
	}
}

// TestHandleIndexIntegrationsLiveLastSyncOmitsDryRunMarker is the negative
// counterpart to TestHandleIndexIntegrationsDryRunLastSyncShowsMarker: a
// real (non-dry-run) successful pass must show its emitted count WITHOUT
// the dry-run marker -- the case an unconditional marker would pass
// clean through.
func TestHandleIndexIntegrationsLiveLastSyncOmitsDryRunMarker(t *testing.T) {
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{Integrations: []IntegrationStatus{{
				ID:         IntegrationLuminar,
				Registered: true,
				LastSync:   &SyncSummary{At: time.Now(), DryRun: false, PairsFound: 5, Emitted: 12, Skipped: 1},
			}}}
		},
		SettingsFunc: func() SettingsView {
			return SettingsView{Integrations: []IntegrationView{{ID: IntegrationLuminar, Enabled: true, DryRun: false}}}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "12 emitted") {
		t.Errorf("response body missing the emitted count\n---\n%s", body)
	}
	if strings.Contains(body, "nothing was emitted") {
		t.Errorf("a real (non-dry-run) sync must not show the dry-run marker\n---\n%s", body)
	}
}

// TestHandleIndexIntegrationsFailedLastSyncShowsError covers a failed sync
// pass -- the error must render, and per TestHandleIndexNeverFabricatesQueueNumbers's
// own invariant applied here, a failed/never-synced pass must never print a
// fabricated "0 emitted".
func TestHandleIndexIntegrationsFailedLastSyncShowsError(t *testing.T) {
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{Integrations: []IntegrationStatus{{
				ID:         IntegrationLuminar,
				Registered: true,
				LastSync:   &SyncSummary{At: time.Now(), Err: errors.New("open catalog: no such file")},
			}}}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "no such file") {
		t.Errorf("response body missing the sync error\n---\n%s", body)
	}
	if strings.Contains(body, "0 emitted") {
		t.Errorf("a failed sync must never fabricate an emitted count\n---\n%s", body)
	}
}

// TestHandleIndexIntegrationsNeverSyncedOmitsLastSync covers an enabled,
// registered integration that hasn't run a pass yet this session
// (LastSync == nil) -- must render config state only, with no "0s ago" /
// "0 emitted" fabrication.
func TestHandleIndexIntegrationsNeverSyncedOmitsLastSync(t *testing.T) {
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{Integrations: []IntegrationStatus{{ID: IntegrationLuminar, Registered: true, LastSync: nil}}}
		},
		SettingsFunc: func() SettingsView {
			return SettingsView{Integrations: []IntegrationView{{ID: IntegrationLuminar, Enabled: true, DryRun: true}}}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "enabled") {
		t.Errorf("response body missing config state for a never-synced integration\n---\n%s", body)
	}
	if strings.Contains(body, "0 emitted") || strings.Contains(body, "ago") {
		t.Errorf("a never-synced integration must not fabricate a last-sync line\n---\n%s", body)
	}
}

// TestHandleIndexHooksEachState covers every DaVinci Resolve hook status
// the status page must distinguish: unregistered, never-checked-yet
// (State == nil, e.g. before tray.go's one-time startup Detect ran),
// no-candidate-directory, not-installed, up-to-date, modified/out-of-date,
// and a failed install.
func TestHandleIndexHooksEachState(t *testing.T) {
	at := time.Now().Add(-90 * time.Second)
	cases := []struct {
		name string
		hs   HookStatus
		want []string
		bad  []string
	}{
		{
			name: "unregistered",
			hs:   HookStatus{ID: HookResolve, Registered: false},
			want: []string{"not configured"},
		},
		{
			name: "never checked yet",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: nil},
			want: []string{"not checked yet this session"},
		},
		{
			name: "no candidate directory",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: &HookState{At: at, Dir: ""}},
			want: []string{"no Scripts/Utility folder found"},
		},
		{
			name: "not installed",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: &HookState{At: at, Dir: "/Users/alice/Library/Application Support/Blackmagic Design/DaVinci Resolve/Fusion/Scripts/Utility", Installed: false}},
			want: []string{"not installed", "would install to"},
		},
		{
			name: "up to date",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: &HookState{At: at, Dir: "/scripts", Path: "/scripts/branchdam_render_hook.py", Installed: true, UpToDate: true}},
			want: []string{"installed and up to date"},
		},
		{
			name: "modified or out of date",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: &HookState{At: at, Dir: "/scripts", Path: "/scripts/branchdam_render_hook.py", Installed: true, UpToDate: false}},
			want: []string{"installed but modified or out of date"},
		},
		{
			name: "failed install",
			hs:   HookStatus{ID: HookResolve, Registered: true, State: &HookState{At: at, Err: errors.New("mkdir /scripts: permission denied")}},
			want: []string{"install failed", "permission denied"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &StatusServer{StatusFunc: func() Status { return Status{Hooks: []HookStatus{tc.hs}} }}
			req := httptest.NewRequest("GET", "/", nil)
			rec := httptest.NewRecorder()
			s.handleIndex(rec, req)
			body := rec.Body.String()
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Errorf("response body missing %q\n---\n%s", want, body)
				}
			}
			for _, bad := range tc.bad {
				if strings.Contains(body, bad) {
					t.Errorf("response body must not contain %q\n---\n%s", bad, body)
				}
			}
		})
	}
}

func TestHandleIndex404sOnUnknownPath(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{} }}
	req := httptest.NewRequest("GET", "/other", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	if rec.Code != 404 {
		t.Errorf("got status %d, want 404", rec.Code)
	}
}

func TestHandleIndexHandshakeOKGreen(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status {
		return Status{HandshakeOK: true, HasDrained: true}
	}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "handshake OK") {
		t.Errorf("response body missing 'handshake OK' when HasDrained && HandshakeOK\n---\n%s", body)
	}
	if strings.Contains(body, "handshake failed") {
		t.Errorf("response body must not contain 'handshake failed' when HasDrained && HandshakeOK\n---\n%s", body)
	}
}

func TestHandleIndexHandshakeNOTReachable(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status {
		return Status{HandshakeOK: false, HasDrained: true}
	}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "handshake failed") {
		t.Errorf("response body missing 'handshake failed' when HasDrained && !HandshakeOK\n---\n%s", body)
	}
	if strings.Contains(body, "handshake OK") {
		t.Errorf("response body must not contain 'handshake OK' when HasDrained && !HandshakeOK\n---\n%s", body)
	}
}

func TestHandleIndexNoDrainYet(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{} }}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "no drain run yet") {
		t.Errorf("response body missing 'no drain run yet' when HasDrained is false\n---\n%s", body)
	}
}

func TestHandleIndexInFlightDrain(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{InFlightDrain: true} }}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "drain in progress") {
		t.Errorf("response body missing 'drain in progress' when InFlightDrain=true\n---\n%s", body)
	}
}

func TestHandleIndexInFlightPrune(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{InFlightPrune: true} }}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "prune in progress") {
		t.Errorf("response body missing 'prune in progress' when InFlightPrune=true\n---\n%s", body)
	}
}

func TestHandleIndexNoInFlightWhenIdle(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{InFlightDrain: false, InFlightPrune: false} }}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "in progress") {
		t.Errorf("response body must not contain 'in progress' when neither drain nor prune is running\n---\n%s", body)
	}
}

func TestHandleIndexBusySinceShown(t *testing.T) {
	since := time.Now().Add(-3 * time.Minute)
	s := &StatusServer{StatusFunc: func() Status {
		return Status{Busy: true, BusyCard: "/media/card1", BusySince: since}
	}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "/media/card1") {
		t.Errorf("response body missing card path\n---\n%s", body)
	}
	if !strings.Contains(body, "ago") {
		t.Errorf("response body missing elapsed time for BusySince\n---\n%s", body)
	}
}

func TestHandleIndexHTMLInjectionPrevented(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status {
		return Status{
			QueueStatus: QueueStatus{
				Configured: true,
				Err:        errors.New("<script>alert('xss')</script>"),
			},
		}
	}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert('xss')</script>") {
		t.Errorf("response body must escape HTML in error messages to prevent injection\n---\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(&#39;xss&#39;)&lt;/script&gt;") {
		t.Errorf("response body should contain escaped HTML entities\n---\n%s", body)
	}
}

func TestHandleIndexRendersIngestProgress(t *testing.T) {
	s := &StatusServer{
		StatusFunc: func() Status {
			return Status{
				Busy:     true,
				BusyCard: "/media/card1",
				IngestProgress: &ingest.ProgressEvent{
					Path:       "/local/DSC_0042.ARW",
					Phase:      ingest.ProgressPhaseCopying,
					BytesDone:  2469606195,
					TotalBytes: 8697308774,
				},
			}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "/local/DSC_0042.ARW") {
		t.Errorf("response body missing file path\n---\n%s", body)
	}
	if !strings.Contains(body, "copying") {
		t.Errorf("response body missing phase\n---\n%s", body)
	}
	if !strings.Contains(body, "2469606195 / 8697308774") {
		t.Errorf("response body missing bytes done/total\n---\n%s", body)
	}
	if !strings.Contains(body, "<progress") {
		t.Errorf("response body missing <progress> element\n---\n%s", body)
	}
}

func TestHandleStatusJSON(t *testing.T) {
	s := &StatusServer{
		Version: "1.2.3",
		StatusFunc: func() Status {
			return Status{
				Busy:     true,
				BusyCard: "/media/card1",
				IngestProgress: &ingest.ProgressEvent{
					Path:       "/local/DSC_0042.ARW",
					Phase:      ingest.ProgressPhaseCopying,
					BytesDone:  2469606195,
					TotalBytes: 8697308774,
				},
			}
		},
	}
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	s.handleStatusJSON(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("got Content-Type %q, want application/json", ct)
	}

	var data struct {
		Version string `json:"version"`
		Status  struct {
			Busy           bool                  `json:"busy"`
			BusyCard       string                `json:"busyCard"`
			IngestProgress *ingest.ProgressEvent `json:"ingestProgress"`
		} `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to decode JSON: %v\n---\n%s", err, rec.Body.String())
	}
	if data.Version != "1.2.3" {
		t.Errorf("got Version=%q, want 1.2.3", data.Version)
	}
	if !data.Status.Busy || data.Status.BusyCard != "/media/card1" {
		t.Errorf("unexpected Status: %+v", data.Status)
	}
	if data.Status.IngestProgress == nil || data.Status.IngestProgress.Path != "/local/DSC_0042.ARW" {
		t.Errorf("unexpected IngestProgress: %+v", data.Status.IngestProgress)
	}
	if data.Status.IngestProgress.BytesDone != 2469606195 || data.Status.IngestProgress.TotalBytes != 8697308774 {
		t.Errorf("unexpected IngestProgress bytes: %+v", data.Status.IngestProgress)
	}
}

func TestHandleIndexAcceptJSON(t *testing.T) {
	s := &StatusServer{
		Version: "1.2.3",
		StatusFunc: func() Status {
			return Status{
				Busy: false,
			}
		},
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("got Content-Type %q, want application/json", ct)
	}
}

func TestHandleIndexRendersPaused(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status {
		return Status{
			Paused: true,
		}
	}}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.handleIndex(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Ingest paused by user") {
		t.Errorf("response body missing 'Ingest paused by user' when Paused=true\n---\n%s", body)
	}
}
