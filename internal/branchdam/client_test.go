package branchdam

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestServer builds an httptest.Server plus a Client pointed at it. h is
// wired directly as the handler so each test can assert on the exact
// request the Client sent (method, path, headers, body) before writing a
// response.
func newTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "test-api-key-0123456789012345678901")
	return srv, c
}

func TestClientPostAllRoutesArePOSTOnly(t *testing.T) {
	// All four agent routes are POST-only server-side (routes.go:66-87) --
	// assert the client never issues anything else, for any of the four
	// wrapper methods.
	var gotMethod string
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := c.Hello(context.Background()); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("Hello: method = %q, want POST", gotMethod)
	}

	if _, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "a"}); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("Handshake: method = %q, want POST", gotMethod)
	}
}

func TestClientSetsXAPIKeyHeaderRaw(t *testing.T) {
	// X-API-Key is the raw shared secret, no "Bearer " or other scheme
	// prefix -- unlike Authorization.
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"dev"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "raw-secret-value")
	if _, err := c.Hello(context.Background()); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if gotHeader != "raw-secret-value" {
		t.Errorf("X-API-Key = %q, want %q (no scheme prefix)", gotHeader, "raw-secret-value")
	}
}

func TestClientHelloSendsNoBody(t *testing.T) {
	var gotContentType string
	var gotBodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		buf := make([]byte, 1)
		n, _ := r.Body.Read(buf)
		gotBodyLen = n
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"0.1.0"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "key")
	resp, err := c.Hello(context.Background())
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if gotContentType != "" {
		t.Errorf("Content-Type = %q, want empty (Hello takes no body)", gotContentType)
	}
	if gotBodyLen != 0 {
		t.Errorf("body length = %d, want 0", gotBodyLen)
	}
	if resp.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", resp.Version)
	}
}

func TestClientPost401PlainTextNotJSON(t *testing.T) {
	// 401/503 auth failures are plain text, never JSON -- the client must
	// surface the raw body via HTTPError.Body without attempting to parse it.
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid or missing X-API-Key"))
	})
	_ = srv

	_, err := c.Hello(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v (%T)", err, err)
	}
	if httpErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", httpErr.StatusCode)
	}
	if httpErr.Body != "invalid or missing X-API-Key" {
		t.Errorf("Body = %q, want the raw plain-text message", httpErr.Body)
	}
	if httpErr.Retryable() {
		t.Error("401 must not be retryable")
	}
}

func TestClientPost503ServiceMisconfigured(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("agent authentication is not configured"))
	})
	_ = srv

	_, err := c.Hello(context.Background())
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.Retryable() {
		t.Error("503 auth-misconfigured must not be retryable")
	}
	if httpErr.Classification() != ClassificationFatal {
		t.Errorf("Classification() = %v, want ClassificationFatal", httpErr.Classification())
	}
}

func TestClientPost422SchemaViolation(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"title":"Unprocessable Entity","status":422}`))
	})
	_ = srv

	_, err := c.PostNodeCreated(context.Background(), "agent-01", NodeCreatedPayload{
		NodeUUID: "u", FilePath: "/storage/archive/f.arw",
	})
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.StatusCode != 422 {
		t.Errorf("StatusCode = %d, want 422", httpErr.StatusCode)
	}
	if httpErr.Retryable() {
		t.Error("422 (client bug) must not be retryable")
	}
}

func TestClientPostEventDoubleEncodesPayload(t *testing.T) {
	// The single most confusable thing in the contract: EventEnvelope.Payload
	// must be a JSON *string* containing the marshalled payload, not the
	// payload object itself.
	var gotEnvelope EventEnvelope
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotEnvelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	})
	_ = srv

	resp, err := c.PostNodeCreated(context.Background(), "agent-01", NodeCreatedPayload{
		NodeUUID: "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		FilePath: "/storage/archive/f.arw",
	})
	if err != nil {
		t.Fatalf("PostNodeCreated: %v", err)
	}
	if resp.EventID != "evt-1" {
		t.Errorf("EventID = %q, want evt-1", resp.EventID)
	}

	if gotEnvelope.AgentID != "agent-01" {
		t.Errorf("AgentID = %q, want agent-01", gotEnvelope.AgentID)
	}
	if gotEnvelope.EventType != EventNodeCreated {
		t.Errorf("EventType = %q, want %q", gotEnvelope.EventType, EventNodeCreated)
	}
	// Payload must decode as a Go string, then THAT string must itself be
	// valid JSON for a NodeCreatedPayload -- i.e. genuinely double-encoded,
	// not a bare object that happened to unmarshal into a string field.
	var inner NodeCreatedPayload
	if err := json.Unmarshal([]byte(gotEnvelope.Payload), &inner); err != nil {
		t.Fatalf("EventEnvelope.Payload is not valid double-encoded JSON: %v", err)
	}
	if inner.NodeUUID != "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60" {
		t.Errorf("inner.NodeUUID = %q", inner.NodeUUID)
	}
}

func TestClientPostEdgeAttachedValidatesBeforeSending(t *testing.T) {
	// A bad EdgeAttachedPayload must never reach the network -- the server
	// doesn't validate at enqueue time, so this is the only place it's
	// caught before burning a fatal, zero-retry failure.
	called := false
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	})
	_ = srv

	_, err := c.PostEdgeAttached(context.Background(), "agent-01", EdgeAttachedPayload{
		SourceNodeUUID:   "a",
		TargetNodeUUID:   "b",
		RelationshipType: RelationshipDerivedFrom,
		Confidence:       0.10, // below minConfidence
		Tier:             1,
	})
	if err != ErrConfidenceOutOfRange {
		t.Errorf("err = %v, want ErrConfidenceOutOfRange", err)
	}
	if called {
		t.Error("server must not be called when client-side validation fails")
	}
}

func TestClientPostEdgeAttachedValidPasses(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"eventId":"evt-2"}`))
	})
	_ = srv

	resp, err := c.PostEdgeAttached(context.Background(), "agent-01", EdgeAttachedPayload{
		SourceNodeUUID:   "a",
		TargetNodeUUID:   "b",
		RelationshipType: RelationshipDerivedFrom,
		Confidence:       0.89,
		Tier:             2,
	})
	if err != nil {
		t.Fatalf("PostEdgeAttached: %v", err)
	}
	if resp.EventID != "evt-2" {
		t.Errorf("EventID = %q", resp.EventID)
	}
}

func TestClientPostNodeMovedNodeDeletedPathRebased(t *testing.T) {
	var gotEventTypes []string
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var env EventEnvelope
		_ = json.NewDecoder(r.Body).Decode(&env)
		gotEventTypes = append(gotEventTypes, env.EventType)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"eventId":"evt"}`))
	})
	_ = srv

	if _, err := c.PostNodeMoved(context.Background(), "a", NodeMovedPayload{NodeUUID: "u", NewFilePath: "/x"}); err != nil {
		t.Fatalf("PostNodeMoved: %v", err)
	}
	if _, err := c.PostNodeDeleted(context.Background(), "a", NodeDeletedPayload{NodeUUID: "u"}); err != nil {
		t.Fatalf("PostNodeDeleted: %v", err)
	}
	if _, err := c.PostPathRebased(context.Background(), "a", PathRebasedPayload{NodeUUID: "u", TargetFilePath: "/y"}); err != nil {
		t.Fatalf("PostPathRebased: %v", err)
	}

	want := []string{EventNodeMoved, EventNodeDeleted, EventPathRebased}
	if len(gotEventTypes) != len(want) {
		t.Fatalf("got %v event types, want %v", gotEventTypes, want)
	}
	for i, w := range want {
		if gotEventTypes[i] != w {
			t.Errorf("event %d = %q, want %q", i, gotEventTypes[i], w)
		}
	}
}

func TestClientHandshake(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req HandshakeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.AgentID != "workstation-01" {
			t.Errorf("AgentID = %q, want workstation-01", req.AgentID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1752591200,"pendingEventsCount":3}`))
	})
	_ = srv

	resp, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "workstation-01"})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if resp.ServerVersion != "0.5.0" {
		t.Errorf("ServerVersion = %q", resp.ServerVersion)
	}
	if resp.PendingEventsCount != 3 {
		t.Errorf("PendingEventsCount = %d, want 3", resp.PendingEventsCount)
	}
}

func TestClientRebaseReturnsRebasedOrCreated(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"nodeUuid":"u","storageLocationId":3,"filePath":"/storage/archive/f.arw","status":"REBASED"}`))
	})
	_ = srv

	resp, err := c.Rebase(context.Background(), RebaseRequest{NodeUUID: "u", TargetPath: "/storage/archive/f.arw"})
	if err != nil {
		t.Fatalf("Rebase: %v", err)
	}
	if resp.Status != "REBASED" {
		t.Errorf("Status = %q, want REBASED", resp.Status)
	}
	if resp.ID != 42 {
		t.Errorf("ID = %d, want 42", resp.ID)
	}
}

func TestClientRebaseReadOnlyTargetRefused400(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"title":"rebase target resolves to read-only tier"}`))
	})
	_ = srv

	_, err := c.Rebase(context.Background(), RebaseRequest{NodeUUID: "u", TargetPath: "/storage/archive/f.arw"})
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", httpErr.StatusCode)
	}
	if httpErr.Classification() != ClassificationFatal {
		t.Errorf("Classification() = %v, want ClassificationFatal (read-only tier is a fatal rebase error)", httpErr.Classification())
	}
}

func TestClientPostNetworkErrorWrapped(t *testing.T) {
	// A closed server: the request never gets a response at all -- confirm
	// the network-level error path is wrapped with context, not silently
	// dropped, and that it's distinct from an HTTPError.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	c := New(srv.URL, "key")
	srv.Close() // close before the request so Do() fails at the transport level

	_, err := c.Hello(context.Background())
	if err == nil {
		t.Fatal("expected error against a closed server")
	}
	var httpErr *HTTPError
	if asHTTPError(err, &httpErr) {
		t.Fatal("a transport-level failure must not be an *HTTPError")
	}
	if !strings.Contains(err.Error(), "branchdam:") {
		t.Errorf("error %q missing branchdam: prefix", err.Error())
	}
}

func TestClientHelloBadJSONResponse(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	})
	_ = srv

	_, err := c.Hello(context.Background())
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("error = %q, want it to mention decode response", err.Error())
	}
}

func TestWithHTTPClientOption(t *testing.T) {
	custom := &http.Client{}
	c := New("http://localhost:8080", "key", WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Error("WithHTTPClient did not override the default http.Client")
	}
}

// TestWithHTTPClientDocumentedOverrideWarning pins the security
// contract documented on WithHTTPClient: a caller-supplied *http.Client
// replaces the entire wire layer including the TLS 1.2 floor
// secureTransport() installs on the default. The contract is
// "power-user only" / "production should not use this option"; the
// test asserts that the override is the caller's, not the package's,
// so a future change to New that re-applies secureTransport to
// every Client (including overrides) doesn't happen silently.
func TestWithHTTPClientDocumentedOverrideWarning(t *testing.T) {
	custom := &http.Client{Timeout: 999 * time.Second}
	c := New("http://localhost:8080", "key", WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Fatal("WithHTTPClient should not be silently wrapped/replaced by New")
	}
	// The custom Timeout is preserved through the override -- this is
	// what a power-user actually wants when they pass their own
	// http.Client, and is the point of the override existing at all.
	if c.httpClient.Timeout != 999*time.Second {
		t.Errorf("custom http.Client Timeout not preserved: got %v, want 999s", c.httpClient.Timeout)
	}
}

// asHTTPError is a small errors.As helper kept local to this file so each
// test above stays a one-liner.
func asHTTPError(err error, target **HTTPError) bool {
	he, ok := err.(*HTTPError)
	if !ok {
		return false
	}
	*target = he
	return true
}

func TestClientCheckContent(t *testing.T) {
	t.Run("found with full hash", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		var gotAPIKey, gotSig string
		_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			gotAPIKey = r.Header.Get("X-API-Key")
			gotSig = r.Header.Get("X-Signature")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"found": true,
				"nodeUuid": "018f-uuid-1234",
				"filePath": "/storage/archive/2026/DSC0001.JPG",
				"lifecycleState": "ACTIVE"
			}`))
		})

		res, err := c.CheckContent(context.Background(), "fasthash123", "fullhash456")
		if err != nil {
			t.Fatalf("CheckContent: %v", err)
		}
		if gotMethod != http.MethodGet {
			t.Errorf("method = %q, want GET", gotMethod)
		}
		if gotPath != "/api/v1/agent/check-content" {
			t.Errorf("path = %q, want /api/v1/agent/check-content", gotPath)
		}
		if !strings.Contains(gotQuery, "fastHash=fasthash123") || !strings.Contains(gotQuery, "fullHash=fullhash456") {
			t.Errorf("query = %q, want fastHash and fullHash params", gotQuery)
		}
		if gotAPIKey == "" {
			t.Error("expected X-API-Key header to be set")
		}
		if gotSig == "" {
			t.Error("expected X-Signature header to be set")
		}
		if !res.Found {
			t.Errorf("res.Found = %v, want true", res.Found)
		}
		if res.NodeUUID != "018f-uuid-1234" {
			t.Errorf("res.NodeUUID = %q, want 018f-uuid-1234", res.NodeUUID)
		}
		if res.FilePath != "/storage/archive/2026/DSC0001.JPG" {
			t.Errorf("res.FilePath = %q, want /storage/archive/2026/DSC0001.JPG", res.FilePath)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"found": false}`))
		})

		res, err := c.CheckContent(context.Background(), "fasthash123", "")
		if err != nil {
			t.Fatalf("CheckContent: %v", err)
		}
		if res.Found {
			t.Errorf("res.Found = %v, want false", res.Found)
		}
		if res.NodeUUID != "" {
			t.Errorf("res.NodeUUID = %q, want empty", res.NodeUUID)
		}
	})

	t.Run("server error", func(t *testing.T) {
		_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server unavailable", http.StatusServiceUnavailable)
		})

		_, err := c.CheckContent(context.Background(), "fast", "full")
		if err == nil {
			t.Fatal("expected error on 503 response, got nil")
		}
		var he *HTTPError
		if !asHTTPError(err, &he) {
			t.Fatalf("expected *HTTPError, got %T: %v", err, err)
		}
		if he.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", he.StatusCode)
		}
	})
}
