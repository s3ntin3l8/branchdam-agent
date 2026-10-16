package branchdam

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Fixture made by branchDAM's own pairing.SealForDevice (the S13 reference
// implementation), not by this package: opening it proves this client
// matches the server's algorithm, not merely itself.
const (
	fixtureCurrentKey = "bdk_fixture_current_key_0123456789abcdefABCDEF"                                                          // pragma: allowlist secret
	fixtureRotatedKey = "bdk_fixture_rotated_key_fedcba9876543210ZYXWVU"                                                          // pragma: allowlist secret
	fixtureSealed     = "v1:1R76zdX0mH2rB3zFFC9SAiRhyuH7DwE4+VqJ46XgxIjomz54uhhxYHa2VBracHGtMtqe/1MsI4+aPq3pMvroAg1Fmsn+57aUREo=" // pragma: allowlist secret
)

func TestOpenRotatedKeyServerFixture(t *testing.T) {
	got, err := OpenRotatedKey(fixtureCurrentKey, fixtureSealed)
	if err != nil {
		t.Fatalf("OpenRotatedKey(server fixture): %v", err)
	}
	if got != fixtureRotatedKey {
		t.Fatalf("OpenRotatedKey = %q, want %q", got, fixtureRotatedKey)
	}
}

func TestOpenRotatedKeyRejects(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fixtureSealed, "v1:"))
	if err != nil {
		t.Fatal(err)
	}
	tampered := slices.Clone(raw)
	tampered[len(tampered)-1] ^= 0x01

	cases := map[string]struct{ key, sealed string }{
		"wrong current key": {"bdk_some_other_key_0123456789abcdefABCDEF", fixtureSealed}, // pragma: allowlist secret
		"empty current key": {"", fixtureSealed},
		"tampered tag":      {fixtureCurrentKey, "v1:" + base64.StdEncoding.EncodeToString(tampered)},
		"missing prefix":    {fixtureCurrentKey, strings.TrimPrefix(fixtureSealed, "v1:")},
		"other version":     {fixtureCurrentKey, "v2:" + strings.TrimPrefix(fixtureSealed, "v1:")},
		"bad base64":        {fixtureCurrentKey, "v1:not base64!"},
		"too short":         {fixtureCurrentKey, "v1:" + base64.StdEncoding.EncodeToString(raw[:20])},
		"empty":             {fixtureCurrentKey, ""},
		"not a header value": {
			fixtureCurrentKey, sealForTest(t, fixtureCurrentKey, "has a space"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := OpenRotatedKey(tc.key, tc.sealed)
			if !errors.Is(err, ErrSealedKeyInvalid) {
				t.Fatalf("OpenRotatedKey = (%q, %v), want ErrSealedKeyInvalid", got, err)
			}
			if got != "" {
				t.Fatalf("OpenRotatedKey returned %q alongside an error", got)
			}
		})
	}
}

func TestOpenRotatedKeyRoundTrip(t *testing.T) {
	for i := 0; i < 8; i++ {
		cur, next := randomKey(t), randomKey(t)
		got, err := OpenRotatedKey(cur, sealForTest(t, cur, next))
		if err != nil || got != next {
			t.Fatalf("round trip %d: got (%q, %v)", i, got, err)
		}
	}
}

// sealForTest is branchDAM's pairing.SealForDevice written out with the
// standard library: AES-256-GCM under HMAC-SHA256(currentKey,
// "branchdam-key-rotation-v1"), "v1:" + base64(nonce || ciphertext || tag).
func sealForTest(t *testing.T, currentKey, newKey string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(currentKey))
	mac.Write([]byte("branchdam-key-rotation-v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return "v1:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(newKey), nil))
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "bdk_" + hex.EncodeToString(b)
}

// rotationServer is a small model of branchDAM's S13 server: keys of one
// pairing, the newest one delivered sealed on a handshake with an older
// one, and adoption -- the first accepted request on the newest key
// revokes every older key.
type rotationServer struct {
	t *testing.T

	mu       sync.Mutex
	ids      map[string]int64 // active key -> key id
	names    map[string]string
	newest   string
	refuse   map[string]bool // keys answered 401 even though active
	undeliv  bool            // no BRANCHDAM_SECRET_KEY: sealedApiKey omitted
	sealWith string          // seal for this key instead of the presented one
	failNext map[string]int  // one-shot status override per key (non-auth failure)
	log      []string
}

func newRotationServer(t *testing.T, keys ...string) (*rotationServer, *httptest.Server) {
	t.Helper()
	rs := &rotationServer{t: t, ids: map[string]int64{}, names: map[string]string{}, refuse: map[string]bool{}, failNext: map[string]int{}}
	for i, k := range keys {
		rs.ids[k] = int64(i + 1)
		rs.names[k] = "k" + string(rune('1'+i))
		rs.newest = k
	}
	srv := httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(srv.Close)
	return rs, srv
}

func (rs *rotationServer) name(k string) string {
	if n, ok := rs.names[k]; ok {
		return n
	}
	return "unknown"
}

func (rs *rotationServer) record(entry string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.log = append(rs.log, entry)
}

func (rs *rotationServer) entries() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return slices.Clone(rs.log)
}

func (rs *rotationServer) serve(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("X-API-Key")
	body, _ := io.ReadAll(r.Body)

	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.log = append(rs.log, r.URL.Path+" "+rs.name(key))

	// The signature must be made with the same key the request presents.
	if r.URL.Path != "/api/v1/agent/upload" {
		want := signRequest(key, r.Method, r.URL.RequestURI(), r.Header.Get("X-Nonce"), r.Header.Get("X-Timestamp"), body)
		if r.Header.Get("X-Signature") != want {
			rs.t.Errorf("%s: X-Signature not made with the presented key", r.URL.Path)
		}
	}

	if status, ok := rs.failNext[key]; ok {
		delete(rs.failNext, key)
		http.Error(w, "unavailable", status)
		return
	}
	id, active := rs.ids[key]
	if !active || rs.refuse[key] {
		http.Error(w, "invalid or missing X-API-Key", http.StatusUnauthorized)
		return
	}
	if key == rs.newest {
		for k := range rs.ids {
			if k != key {
				delete(rs.ids, k) // adoption revokes every older key
			}
		}
	}

	switch r.URL.Path {
	case handshakePath:
		out := map[string]any{"ok": true, "serverVersion": "test", "currentKeyId": id}
		if key != rs.newest {
			sealed := ""
			if !rs.undeliv {
				sealFor := key
				if rs.sealWith != "" {
					sealFor = rs.sealWith
				}
				sealed = sealForTest(rs.t, sealFor, rs.newest)
			}
			out["pendingRotation"] = map[string]any{
				"keyId": rs.ids[rs.newest], "apiKey": "", "sealedApiKey": sealed, "previousKeyExpiresAtUnix": 1900000000,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	case "/api/v1/agent/upload":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"nodeUuid":"n"}`))
	default:
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"e"}`))
	}
}

// recordingPersister is a KeyPersister that logs into the server's log, so
// tests can check a key is stored before it is first sent.
func recordingPersister(rs *rotationServer, stored *[]string, fail error) KeyPersister {
	return func(_ context.Context, apiKey string) error {
		rs.record("persist " + rs.name(apiKey))
		if fail != nil {
			return fail
		}
		*stored = append(*stored, apiKey)
		return nil
	}
}

func postSomething(t *testing.T, c *Client) error {
	t.Helper()
	_, err := c.PostNodeDeleted(context.Background(), "agent-1", NodeDeletedPayload{NodeUUID: "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60"})
	return err
}

func TestHandshakeAdoptsRotatedKey(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	var stored []string
	c := New(srv.URL, k1, WithKeyPersister(recordingPersister(rs, &stored, nil)))

	hs, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if hs.CurrentKeyID != 2 || hs.PendingRotation != nil {
		t.Fatalf("returned handshake = currentKeyId %d, pendingRotation %v; want the confirming handshake on key 2", hs.CurrentKeyID, hs.PendingRotation)
	}
	if !slices.Equal(stored, []string{k2}) {
		t.Fatalf("persisted %d keys, want exactly the rotated one", len(stored))
	}
	want := []string{handshakePath + " k1", "persist k2", handshakePath + " k2"}
	if got := rs.entries(); !slices.Equal(got, want) {
		t.Fatalf("sequence = %v, want %v (stored before first use)", got, want)
	}
	if c.currentKey() != k2 {
		t.Fatal("client is not on the rotated key")
	}

	// Later requests use the new key; the old one is revoked server-side.
	if err := postSomething(t, c); err != nil {
		t.Fatalf("request after adoption: %v", err)
	}
	if got := rs.entries(); got[len(got)-1] != "/api/v1/agent/events k2" {
		t.Fatalf("last request = %q, want events on k2", got[len(got)-1])
	}
	if _, active := rs.ids[k1]; active {
		t.Fatal("old key still active after adoption")
	}
}

func TestHandshakeRotationNotAdopted(t *testing.T) {
	cases := map[string]struct {
		setup     func(rs *rotationServer, k1 string)
		persister bool
		failStore error
		persists  int
	}{
		"no persister":           {persister: false},
		"store fails":            {persister: true, failStore: errors.New("disk full"), persists: 1},
		"undeliverable":          {persister: true, setup: func(rs *rotationServer, _ string) { rs.undeliv = true }},
		"sealed for another key": {persister: true, setup: func(rs *rotationServer, _ string) { rs.sealWith = "bdk_not_this_devices_key_0123456789" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k1, k2 := randomKey(t), randomKey(t)
			rs, srv := newRotationServer(t, k1, k2)
			if tc.setup != nil {
				tc.setup(rs, k1)
			}
			var stored []string
			var opts []Option
			if tc.persister {
				opts = append(opts, WithKeyPersister(recordingPersister(rs, &stored, tc.failStore)))
			}
			c := New(srv.URL, k1, opts...)

			hs, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"})
			if err != nil {
				t.Fatalf("Handshake: %v", err)
			}
			if hs.CurrentKeyID != 1 || hs.PendingRotation == nil {
				t.Fatalf("want the first handshake's response back, got currentKeyId %d", hs.CurrentKeyID)
			}
			if c.currentKey() != k1 {
				t.Fatal("client left its current key without a stored replacement")
			}
			persists := 0
			for _, e := range rs.entries() {
				if strings.HasPrefix(e, "persist") {
					persists++
				}
				if strings.HasSuffix(e, " k2") && !strings.HasPrefix(e, "persist") {
					t.Fatalf("rotated key was sent: %v", rs.entries())
				}
			}
			if persists != tc.persists {
				t.Fatalf("persist calls = %d, want %d", persists, tc.persists)
			}
			if err := postSomething(t, c); err != nil {
				t.Fatalf("old key must keep working: %v", err)
			}
		})
	}
}

func TestRotatedKeyRefusedFallsBackOnce(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	rs.refuse[k2] = true
	var stored []string
	c := New(srv.URL, k1, WithKeyPersister(recordingPersister(rs, &stored, nil)))

	hs, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if hs.CurrentKeyID != 1 {
		t.Fatalf("currentKeyId = %d, want 1 (the retry on the old key)", hs.CurrentKeyID)
	}
	want := []string{
		handshakePath + " k1", "persist k2", handshakePath + " k2", // refused
		handshakePath + " k1", "persist k1", // retried on the old key, which is written back
	}
	if got := rs.entries(); !slices.Equal(got, want) {
		t.Fatalf("sequence = %v, want %v", got, want)
	}
	if !slices.Equal(stored, []string{k2, k1}) {
		t.Fatal("the old key was not written back after the rotated key was refused")
	}
	if c.currentKey() != k1 {
		t.Fatal("client is not back on the old key")
	}

	// Offered the same refused key again: not re-adopted.
	if _, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"}); err != nil {
		t.Fatalf("second Handshake: %v", err)
	}
	if got := rs.entries(); len(got) != len(want)+1 || got[len(got)-1] != handshakePath+" k1" {
		t.Fatalf("refused key retried: %v", got)
	}
}

// A transient failure on the old-key retry must not lose the write-back: the
// old key is stored again on the next request the server accepts.
func TestRotatedKeyFallbackSurvivesTransientFailure(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	rs.refuse[k2] = true
	var stored []string
	persist := recordingPersister(rs, &stored, nil)
	c := New(srv.URL, k1, WithKeyPersister(func(ctx context.Context, apiKey string) error {
		if apiKey == k2 { // pragma: allowlist secret
			rs.mu.Lock()
			rs.failNext[k1] = http.StatusServiceUnavailable
			rs.mu.Unlock()
		}
		return persist(ctx, apiKey)
	}))

	// The confirming handshake is best-effort, so Handshake itself may succeed.
	_, _ = c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"})
	want := []string{handshakePath + " k1", "persist k2", handshakePath + " k2", handshakePath + " k1"} // 503
	if got := rs.entries(); !slices.Equal(got, want) {
		t.Fatalf("sequence = %v, want %v", got, want)
	}
	if c.currentKey() != k1 {
		t.Fatal("client is not back on the old key")
	}
	if !slices.Equal(stored, []string{k2}) {
		t.Fatalf("stored %d keys before any accepted old-key request, want only the rotated one", len(stored))
	}

	if err := postSomething(t, c); err != nil {
		t.Fatalf("post on the old key: %v", err)
	}
	if !slices.Equal(stored, []string{k2, k1}) {
		t.Fatal("the old key was not written back after the transient failure")
	}
}

func TestRotatedKeyBothRefusedKeepsBothKeys(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	var stored []string
	persist := recordingPersister(rs, &stored, nil)
	c := New(srv.URL, k1, WithKeyPersister(func(ctx context.Context, apiKey string) error {
		err := persist(ctx, apiKey)
		// The pairing is revoked between storing and using the new key.
		rs.mu.Lock()
		rs.refuse[k1], rs.refuse[k2] = true, true
		rs.mu.Unlock()
		return err
	}))

	if _, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"}); err != nil {
		t.Fatalf("Handshake: %v (the first handshake succeeded)", err)
	}
	// The store holds the new key, memory the old one: neither is lost.
	if !slices.Equal(stored, []string{k2}) {
		t.Fatalf("store = %d keys, want only the rotated key (not overwritten by an unverified fallback)", len(stored))
	}
	if c.currentKey() != k1 {
		t.Fatal("client should be back on the old key")
	}
}

// TestUploadRefusalRevertsKey: when the confirming handshake does not get
// an answer, the next request adopts. An upload refused on the new key
// cannot be replayed, but it still sends the client back to the old key,
// and the old key is written back once the server accepts it.
func TestUploadRefusalRevertsKey(t *testing.T) {
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	rs.failNext[k2] = http.StatusServiceUnavailable
	var stored []string
	c := New(srv.URL, k1, WithKeyPersister(recordingPersister(rs, &stored, nil)))

	if _, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-1"}); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if c.currentKey() != k2 {
		t.Fatal("client should stay on the stored rotated key when the confirmation got no answer")
	}

	rs.mu.Lock()
	rs.refuse[k2] = true
	rs.mu.Unlock()
	_, err := c.Upload(context.Background(), bytes.NewReader([]byte("data")), UploadOptions{Filename: "a.jpg"})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Upload err = %v, want 401", err)
	}
	if c.currentKey() != k1 {
		t.Fatal("refused upload did not revert the client to the old key")
	}
	if err := postSomething(t, c); err != nil {
		t.Fatalf("request on the old key: %v", err)
	}
	if !slices.Equal(stored, []string{k2, k1}) {
		t.Fatal("old key not written back after the server accepted it")
	}
}

func TestStaleKeyRequestRetriedWithCurrentKey(t *testing.T) {
	// A request that left with the old key after the client switched gets
	// one retry with the current key.
	k1, k2 := randomKey(t), randomKey(t)
	rs, srv := newRotationServer(t, k1, k2)
	c := New(srv.URL, k1)
	delete(rs.ids, k1) // already revoked server-side
	c.keyMu.Lock()
	c.apiKey = k2 // pragma: allowlist secret
	c.keyMu.Unlock()
	if retry := c.observeAuth(context.Background(), k1, http.StatusUnauthorized); retry != k2 {
		t.Fatal("stale-key 401 should be retried with the current key")
	}
	if retry := c.observeAuth(context.Background(), k2, http.StatusUnauthorized); retry != "" {
		t.Fatal("a 401 on the current key must not be retried")
	}
}
