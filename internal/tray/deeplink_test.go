package tray

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testDeepLinkURL = "branchdam://?server=https%3A%2F%2Fdam.example.com&key=01234567890123456789012345678901&agent=dev-x123"

const testDeepLinkKey = "01234567890123456789012345678901"

type deepLinkRecorder struct {
	confirmCalls int
	confirmBody  string
	notes        []string
}

func (r *deepLinkRecorder) confirm(answer bool) func(context.Context, string, string) bool {
	return func(_ context.Context, _, body string) bool {
		r.confirmCalls++
		r.confirmBody = body
		return answer
	}
}

func (r *deepLinkRecorder) notify(_ context.Context, _, msg string) { r.notes = append(r.notes, msg) }

func TestHandleDeepLinkPairsAfterConfirm(t *testing.T) {
	settings := &spySettings{}
	rec := &deepLinkRecorder{}
	handleDeepLink(context.Background(), testDeepLinkURL, settings, rec.confirm(true), rec.notify)

	if rec.confirmCalls != 1 {
		t.Fatalf("confirm called %d times, want 1", rec.confirmCalls)
	}
	if settings.lastStringKey != "pair" || settings.lastStringVal != "https://dam.example.com" {
		t.Errorf("Pair not invoked with parsed server: key=%q val=%q", settings.lastStringKey, settings.lastStringVal)
	}
	if !strings.Contains(rec.confirmBody, "dam.example.com") || !strings.Contains(rec.confirmBody, "dev-x123") {
		t.Errorf("confirm body %q must show server and agent", rec.confirmBody)
	}
	if len(rec.notes) != 1 || !strings.Contains(rec.notes[0], "Paired") {
		t.Errorf("notes = %v, want one success note", rec.notes)
	}
}

func TestHandleDeepLinkDeclinedDoesNotPair(t *testing.T) {
	settings := &spySettings{}
	rec := &deepLinkRecorder{}
	handleDeepLink(context.Background(), testDeepLinkURL, settings, rec.confirm(false), rec.notify)

	if settings.lastStringKey == "pair" {
		t.Error("Pair was called despite the operator declining")
	}
	if len(rec.notes) != 0 {
		t.Errorf("notes = %v, want none on decline", rec.notes)
	}
}

func TestHandleDeepLinkNilConfirmFailsClosed(t *testing.T) {
	settings := &spySettings{}
	rec := &deepLinkRecorder{}
	handleDeepLink(context.Background(), testDeepLinkURL, settings, nil, rec.notify)

	if settings.lastStringKey == "pair" {
		t.Error("Pair was called with no confirmation dialog wired")
	}
	if len(rec.notes) != 1 || !strings.Contains(rec.notes[0], "Pairing failed") {
		t.Errorf("notes = %v, want one failure note", rec.notes)
	}
}

// The confirmation must be unconditional: it is the only thing between a
// malicious web page and a re-pointed agent, so no ConfirmDestructive-style
// setting may bypass it.
func TestHandleDeepLinkAlwaysConfirmsRegardlessOfSettings(t *testing.T) {
	settings := &spySettings{}
	rec := &deepLinkRecorder{}
	handleDeepLink(context.Background(), testDeepLinkURL, settings, rec.confirm(false), rec.notify)
	if rec.confirmCalls != 1 {
		t.Errorf("confirm called %d times, want exactly 1", rec.confirmCalls)
	}
}

func TestHandleDeepLinkInvalidURLNotifiesWithoutKey(t *testing.T) {
	settings := &spySettings{}
	rec := &deepLinkRecorder{}
	bad := "branchdm://?server=https%3A%2F%2Fdam.example.com&key=" + testDeepLinkKey
	handleDeepLink(context.Background(), bad, settings, rec.confirm(true), rec.notify)

	if rec.confirmCalls != 0 || settings.lastStringKey == "pair" {
		t.Error("an invalid URL must not reach confirm or Pair")
	}
	if len(rec.notes) != 1 {
		t.Fatalf("notes = %v, want one failure note", rec.notes)
	}
	if strings.Contains(rec.notes[0], testDeepLinkKey) {
		t.Errorf("failure note leaks the API key: %q", rec.notes[0])
	}
}

func TestHandleDeepLinkPairErrorNotifiesWithoutKey(t *testing.T) {
	settings := &spySettings{setErr: errors.New("server rejected the key")}
	rec := &deepLinkRecorder{}
	handleDeepLink(context.Background(), testDeepLinkURL, settings, rec.confirm(true), rec.notify)

	if len(rec.notes) != 1 || !strings.Contains(rec.notes[0], "Pairing failed") {
		t.Fatalf("notes = %v, want one failure note", rec.notes)
	}
	if strings.Contains(rec.notes[0]+rec.confirmBody, testDeepLinkKey) {
		t.Error("API key leaked into notify/confirm text")
	}
}

func deepLinkRequest(t *testing.T, s *StatusServer, source string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	body, _ := json.Marshal(pairActionRequest{URL: testDeepLinkURL, Source: source})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/pair", body))
	return rec
}

func TestHandleActionPairDeepLinkConfirmsAsync(t *testing.T) {
	settings := &spySettings{}
	done := make(chan string, 1)
	confirmed := make(chan struct{}, 1)
	s := &StatusServer{
		Addr: "127.0.0.1:38080", Token: "tok", Settings: settings,
		PairConfirm: func(context.Context, string, string) bool { confirmed <- struct{}{}; return true },
		PairNotify:  func(_ context.Context, _, msg string) { done <- msg },
	}
	rec := deepLinkRequest(t, s, PairSourceDeepLink)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var res pairActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || !res.OK || !res.Pending {
		t.Errorf("response = %s, want ok+pending", rec.Body.String())
	}
	select {
	case <-confirmed:
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation dialog was never shown")
	}
	select {
	case msg := <-done:
		if !strings.Contains(msg, "Paired") {
			t.Errorf("notify = %q, want success", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no outcome notification")
	}
	if settings.lastStringKey != "pair" {
		t.Error("Pair not called after confirmation")
	}
}

func TestHandleActionPairDeepLinkWithoutConfirmIs503(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Settings: settings}
	rec := deepLinkRequest(t, s, PairSourceDeepLink)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if settings.lastStringKey == "pair" {
		t.Error("Pair was called with no confirmation dialog wired")
	}
}

// A UI-originated pair (no source) was already confirmed by the operator's
// own click and must stay synchronous, with no second dialog.
func TestHandleActionPairWithoutSourceSkipsConfirm(t *testing.T) {
	settings := &spySettings{}
	s := &StatusServer{
		Addr: "127.0.0.1:38080", Token: "tok", Settings: settings,
		PairConfirm: func(context.Context, string, string) bool { t.Error("confirm must not run for UI pairs"); return false },
	}
	rec := deepLinkRequest(t, s, "")
	if rec.Code != http.StatusOK || settings.lastStringKey != "pair" {
		t.Errorf("status = %d, pair key = %q, want synchronous 200 pair", rec.Code, settings.lastStringKey)
	}
}
