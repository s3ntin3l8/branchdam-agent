package tray

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatusServerRejectsNonLoopbackHost is the DNS-rebinding regression
// test: a request reaching the loopback listener with an attacker's
// hostname in Host must not get /status.json.
func TestStatusServerRejectsNonLoopbackHost(t *testing.T) {
	s := NewStatusServer("127.0.0.1:0", func() Status { return Status{WatchDirs: []string{"/secret/path"}} },
		func() SettingsView { return SettingsView{} }, "1.0.0")
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() {
		// The transport can dial a spare keep-alive connection it never
		// sends a request on; Shutdown treats such a StateNew connection
		// as active for up to 5s. Closing the client's idle connections
		// first lets Serve return at once, and the wait below is clearly
		// longer than Serve's own shutdown bound, so the two timers never
		// race.
		client.CloseIdleConnections()
		cancel()
		select {
		case <-done:
		case <-time.After(statusServerShutdownTimeout + 5*time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1:" + port, http.StatusOK},
		{"localhost:" + port, http.StatusOK},
		{"LOCALHOST:" + port, http.StatusOK},
		{"[::1]:" + port, http.StatusOK},
		{":" + port, http.StatusOK},
		{"attacker.example:" + port, http.StatusForbidden},
		{"attacker.example", http.StatusForbidden},
		{"127.0.0.1", http.StatusForbidden},
		{"127.0.0.1:1", http.StatusForbidden},
		{"192.168.1.5:" + port, http.StatusForbidden},
		{"localhost.attacker.example:" + port, http.StatusForbidden},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/status.json", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Host %q: %v", tc.host, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("Host %q: status %d, want %d", tc.host, resp.StatusCode, tc.want)
		}
	}
}

// TestStatusServerHostCheckSkippedForWidenedBind: an operator who set a
// non-loopback statusAddr reaches the read-only page by the machine's
// network name, which the server cannot enumerate.
func TestStatusServerHostCheckSkippedForWidenedBind(t *testing.T) {
	s := &StatusServer{Addr: "0.0.0.0:38080", StatusFunc: func() Status { return Status{} }}
	req := httptest.NewRequest(http.MethodGet, "/status.json", nil)
	req.Host = "workstation.lan:38080"
	rec := httptest.NewRecorder()
	s.handler("38080").ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status %d, want 200 for a widened bind", rec.Code)
	}
}
