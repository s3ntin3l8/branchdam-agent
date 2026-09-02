package branchdam

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewRefusesCleartextNonLoopback pins the S-2 defense-in-depth layer:
// internal/config.Validate already rejects http:// on a non-loopback host
// at config-load time (issue #96), but a program that constructs a Client
// directly (a test, an SDK consumer, a future subcommand) must also fail
// fast rather than send the X-API-Key over a cleartext wire.
func TestNewRefusesCleartextNonLoopback(t *testing.T) {
	cases := []string{
		"http://branchdam.example.com",
		"http://10.0.0.5",
		"http://192.168.1.10",
	}
	for _, baseURL := range cases {
		t.Run(baseURL, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("New(%q) did not panic; expected panic for http:// on non-loopback host", baseURL)
				}
			}()
			_ = New(baseURL, "0123456789abcdef0123456789abcdef")
		})
	}
}

// TestNewAllowsCleartextLoopback pins the loopback carve-out: a developer
// pointing at a co-located dev server (http://localhost:8080) keeps
// working without having to reach for TLS first. Internal config.Validate
// emits a warning for the same case (#96) -- the Client itself must not
// refuse it.
func TestNewAllowsCleartextLoopback(t *testing.T) {
	for _, baseURL := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		t.Run(baseURL, func(t *testing.T) {
			c := New(baseURL, "0123456789abcdef0123456789abcdef")
			if c == nil {
				t.Fatalf("New(%q) returned nil for loopback http", baseURL)
			}
		})
	}
}

// TestNewAllowsHTTPS pins the production-scheme path: https on any host
// (loopback or not) is always accepted.
func TestNewAllowsHTTPS(t *testing.T) {
	for _, baseURL := range []string{
		"https://branchdam.example.com",
		"https://localhost:8443",
		"https://10.0.0.5:443",
	} {
		t.Run(baseURL, func(t *testing.T) {
			c := New(baseURL, "0123456789abcdef0123456789abcdef")
			if c == nil {
				t.Fatalf("New(%q) returned nil", baseURL)
			}
		})
	}
}

// TestClientAddsReplayProtectionHeaders pins S-3: every outgoing request
// must carry X-Timestamp, X-Nonce, and X-Signature, with the signature
// computed as HMAC-SHA256 over the documented canonical string using a
// key derived from the apiKey. The server-side validator is a follow-up
// (a separate branchDAM PR gated on server.signedRequests) -- the client
// contract is what this test pins.
func TestClientAddsReplayProtectionHeaders(t *testing.T) {
	const apiKey = "0123456789abcdef0123456789abcdef"
	var gotTimestamp, gotNonce, gotSignature string
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimestamp = r.Header.Get("X-Timestamp")
		gotNonce = r.Header.Get("X-Nonce")
		gotSignature = r.Header.Get("X-Signature")
		gotPath = r.URL.Path
		// Hello sends no body; drain anyway so any future change that
		// gives it one still exercises the body path here.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"dev"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, apiKey)
	if _, err := c.Hello(context.Background()); err != nil {
		t.Fatalf("Hello: %v", err)
	}

	if gotTimestamp == "" {
		t.Error("X-Timestamp header missing")
	}
	if gotNonce == "" {
		t.Error("X-Nonce header missing")
	}
	if gotSignature == "" {
		t.Error("X-Signature header missing")
	}

	sig, err := hex.DecodeString(gotSignature)
	if err != nil {
		t.Fatalf("X-Signature is not valid hex: %v", err)
	}
	if len(sig) != sha256.Size {
		t.Errorf("X-Signature length = %d, want %d (SHA256)", len(sig), sha256.Size)
	}

	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("POST\n" + gotPath + "\n" + gotNonce + "\n" + gotTimestamp + "\n"))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if gotSignature != wantSig {
		t.Errorf("X-Signature = %q, want HMAC-SHA256(apiKey, canonical) = %q", gotSignature, wantSig)
	}
}

// TestClientReplaySignatureIncludesBody pins that a non-empty request
// body is mixed into the signature -- otherwise an attacker could swap
// a POST /events payload without invalidating the signature.
func TestClientReplaySignatureIncludesBody(t *testing.T) {
	const apiKey = "0123456789abcdef0123456789abcdef"
	var gotTimestamp, gotNonce, gotSignature string
	var gotPath string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimestamp = r.Header.Get("X-Timestamp")
		gotNonce = r.Header.Get("X-Nonce")
		gotSignature = r.Header.Get("X-Signature")
		gotPath = r.URL.Path
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, apiKey)
	if _, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "workstation-01"}); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	if gotSignature == "" {
		t.Fatal("X-Signature header missing")
	}
	if len(gotBody) == 0 {
		t.Fatal("test bug: server saw no body to include in the signature")
	}

	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte("POST\n" + gotPath + "\n" + gotNonce + "\n" + gotTimestamp + "\n"))
	mac.Write(gotBody)
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if gotSignature != wantSig {
		t.Errorf("X-Signature did not match HMAC over canonical+body: got %q, want %q", gotSignature, wantSig)
	}
}

// TestClientNonceIsUniquePerRequest pins that the nonce isn't a fixed
// string or a counter that an attacker could predict -- each request
// gets a fresh one.
func TestClientNonceIsUniquePerRequest(t *testing.T) {
	var seenNonces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenNonces = append(seenNonces, r.Header.Get("X-Nonce"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"dev"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "0123456789abcdef0123456789abcdef")
	for i := 0; i < 5; i++ {
		if _, err := c.Hello(context.Background()); err != nil {
			t.Fatalf("Hello iter %d: %v", i, err)
		}
	}
	if len(seenNonces) != 5 {
		t.Fatalf("got %d nonces, want 5", len(seenNonces))
	}
	seen := map[string]bool{}
	for _, n := range seenNonces {
		if n == "" {
			t.Error("a request had an empty X-Nonce")
		}
		if seen[n] {
			t.Errorf("nonce %q appeared twice across 5 requests", n)
		}
		seen[n] = true
	}
}

// TestClientHTTPErrorMessageDoesNotEchoBody pins S-7: a non-2xx response
// body must not be inlined verbatim into HTTPError.Error(). A server
// that echoes the operator's submitted payload back (e.g. echoing a
// stored X-API-Key prefix) would otherwise be re-leaked into logs and
// crash dumps. The status code stays; the body is preserved as a field
// (Classification() reads it for fatal/transient matching) but is not
// printed by Error().
func TestClientHTTPErrorMessageDoesNotEchoBody(t *testing.T) {
	sensitiveBody := "internal note: stored key prefix stored_key_abcd1234 looks suspicious"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(sensitiveBody))
	}))
	defer srv.Close()

	c := New(srv.URL, "0123456789abcdef0123456789abcdef")
	_, err := c.Hello(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), sensitiveBody) {
		t.Errorf("HTTPError.Error() echoed the response body verbatim: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("HTTPError.Error() missing the status code: %q", err.Error())
	}
}

// TestClientHTTPErrorMessageDoesNotEchoAPIKey is the focused half of S-7:
// a server response that names or reflects the X-API-Key must not reach
// the client's error message. The apiKey here is the one the test
// injected; the simulated response echoes it back to mimic a verbose
// debug response.
func TestClientHTTPErrorMessageDoesNotEchoAPIKey(t *testing.T) {
	const apiKey = "0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("X-API-Key " + apiKey + " is invalid"))
	}))
	defer srv.Close()

	c := New(srv.URL, apiKey)
	_, err := c.Hello(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Errorf("HTTPError.Error() leaked the apiKey: %q", err.Error())
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if !strings.Contains(httpErr.Body, apiKey) {
		t.Errorf("HTTPError.Body lost its content: %q", httpErr.Body)
	}
}

// TestClientUsesTLS12MinimumTransport pins the S-2 follow-on: NewClient
// constructs an http.Transport with tls.Config{MinVersion: tls.VersionTLS12}
// so the Go default (which permits TLS 1.0/1.1) cannot be silently
// downgraded by a future option that's added without the same gate.
// Directly inspects the Transport the Client constructed.
func TestClientUsesTLS12MinimumTransport(t *testing.T) {
	c := New("https://branchdam.example.com", "0123456789abcdef0123456789abcdef")
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Client.httpClient.Transport = %T, want *http.Transport", c.httpClient.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("Client.httpClient.Transport.TLSClientConfig is nil")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLS MinVersion = %d, want tls.VersionTLS12 (%d)", tr.TLSClientConfig.MinVersion, tls.VersionTLS12)
	}
}
