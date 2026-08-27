package tray

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	s := NewStatusServer(":1234", func() Status { return Status{} }, "1.2.3")
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
				QueueStatus: QueueStatusStub,
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
	for _, want := range []string{"9.9.9", "/media/card1", QueueStatusStub, "disabled", "3 submitted"} {
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

func TestListenIsTheSingleInstanceGuard(t *testing.T) {
	s1 := NewStatusServer("127.0.0.1:0", func() Status { return Status{} }, "1.0.0")
	ln1, err := s1.Listen()
	if err != nil {
		t.Fatalf("first Listen() failed: %v", err)
	}
	defer ln1.Close()

	s2 := NewStatusServer(ln1.Addr().String(), func() Status { return Status{} }, "1.0.0")
	if _, err := s2.Listen(); err == nil {
		t.Error("expected a second Listen() on the same address to fail -- this is the single-instance guard a self-update relaunch relies on")
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
