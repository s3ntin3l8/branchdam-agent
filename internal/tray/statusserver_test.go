package tray

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestMuxRootReturnsJSON is the regression test for the UX rethink: the
// browsable HTML status page (assets/index.html, handleIndex) is gone --
// "/" now serves the same JSON view as "/status" and "/status.json", so
// there is exactly one status/settings surface (the Wails window) plus one
// machine-readable endpoint for anyone who wants to curl it.
func TestMuxRootReturnsJSON(t *testing.T) {
	s := &StatusServer{
		Version:    "1.2.3",
		StatusFunc: func() Status { return Status{} },
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("got Content-Type %q, want application/json", ct)
	}
	var data struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to decode JSON: %v\n---\n%s", err, rec.Body.String())
	}
	if data.Version != "1.2.3" {
		t.Errorf("got Version=%q, want 1.2.3", data.Version)
	}
}

// TestMuxUnknownPath404s proves "/{$}" (the exact-root pattern) preserved
// handleIndex's old "path != /" guard rather than silently matching every
// path -- ServeMux's own routing does the work handleIndex used to do by
// hand.
func TestMuxUnknownPath404s(t *testing.T) {
	s := &StatusServer{StatusFunc: func() Status { return Status{} }}
	req := httptest.NewRequest("GET", "/other", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("got status %d, want 404", rec.Code)
	}
}
