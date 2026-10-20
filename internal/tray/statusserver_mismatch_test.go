package tray

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// routableAddr is a net.Addr that reports a fixed host:port string -- the
// shape a real listener's Addr() has, so net.SplitHostPort parses it.
type routableAddr string

func (a routableAddr) Network() string { return "tcp" }
func (a routableAddr) String() string  { return string(a) }

// routableListener wraps a real listener and reports a DIFFERENT address
// from Addr(): the listener really accepts connections on loopback, but
// the address it claims is routable.
//
// This is the M8 mismatch state, made mechanical: "localhost" is a NAME,
// net.Listen resolves it through the host's resolver, and on a machine
// whose resolver maps it to a routable address the listener lands on a
// routable interface while the configured string still reads "localhost".
type routableListener struct {
	net.Listener
	addr net.Addr
}

func (l *routableListener) Addr() net.Addr { return l.addr }

// TestServeMismatchBindDropsTheAPIRoutes drives the mismatch case END TO
// END through Serve -- the one path that was untested.
//
// The whole safety argument rests on Serve recording the bind BEFORE it
// builds the handler: s.boundAddr = bound has to happen before
// s.handler(port) is called, because registerAPIRoutes reads it while the
// route table is being built. Nothing else pins that ordering -- the
// predicate tests assign boundAddr by hand, and the one test that calls
// Serve binds loopback for real, so the mismatch never goes through it.
//
// So this test runs Serve against a listener that reports a routable
// address and asserts the consequence over real HTTP:
//
//   - a LAN Host gets 403 on /status.json -- the Host gate follows the
//     CONFIGURED address, so it survives a routable bind,
//   - a loopback Host gets 200 -- the read-only page still answers, and
//   - GET /api/status gets 404 -- the route was never REGISTERED.
//
// The 404 is the assertion with teeth. If the boundAddr assignment moves
// after the handler is built, the /api/* route table is registered
// against a routable listener and the same request answers 401 (the token
// gate rejecting an unauthenticated caller) instead of 404: the mutating
// surface back on a routable interface, exactly what M8 exists to
// prevent.
func TestServeMismatchBindDropsTheAPIRoutes(t *testing.T) {
	realLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := &routableListener{Listener: realLn, addr: routableAddr("192.168.1.5:38080")}

	// The configured address says loopback; the bind says routable.
	s := &StatusServer{
		Addr:       "localhost:38080",
		StatusFunc: func() Status { return Status{} },
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

	// The URL addresses the real loopback listener; the Host header is
	// what the server's gate reads, so the two are set separately.
	base := "http://" + realLn.Addr().String()
	waitUntilServing(t, client, base)

	// A LAN name is refused: the Host gate survived the routable bind.
	resp := getWithHost(t, client, base+"/status.json", "workstation.lan:38080")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /status.json with a LAN Host = %d, want %d -- the Host gate must survive a routable bind",
			resp.StatusCode, http.StatusForbidden)
	}

	// A loopback Host still reads the page.
	resp = getWithHost(t, client, base+"/status.json", "127.0.0.1:38080")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /status.json with a loopback Host = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// The /api/* surface is gone: the routable bind disqualified it, so
	// the route was never registered.
	resp = getWithHost(t, client, base+"/api/status", "127.0.0.1:38080")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/status = %d, want %d -- the /api/* routes must not be registered on a routable bind",
			resp.StatusCode, http.StatusNotFound)
	}
}

// getWithHost issues a real GET with an explicit Host header -- the Host
// header is what the server's gate reads, so it is set apart from the
// URL that actually addresses the listener.
func getWithHost(t *testing.T, client *http.Client, url, host string) *http.Response {
	t.Helper()
	resp, err := doWithHost(client, url, host)
	if err != nil {
		t.Fatalf("GET %s with Host %q: %v", url, host, err)
	}
	return resp
}

// doWithHost is getWithHost without the fatal, so the readiness poll can
// retry on a transport error.
func doWithHost(client *http.Client, url, host string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	return client.Do(req)
}

// waitUntilServing polls the listener until it answers at all. Serve
// starts accepting in a goroutine the test does not synchronise with, so
// the first request can land a moment early; any HTTP answer counts as
// "serving" -- the status assertions are what check the answer.
func waitUntilServing(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := doWithHost(client, base+"/status.json", "127.0.0.1:38080")
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the status server never answered on %s: %v", base, lastErr)
}
