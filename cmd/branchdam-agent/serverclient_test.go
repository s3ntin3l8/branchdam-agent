package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// Made by branchDAM's pairing.SealForDevice: rotatedKey sealed for
// currentKey (same fixture as internal/branchdam's keyrotation_test.go).
const (
	rotCurrentKey = "bdk_fixture_current_key_0123456789abcdefABCDEF"                                                          // pragma: allowlist secret
	rotNewKey     = "bdk_fixture_rotated_key_fedcba9876543210ZYXWVU"                                                          // pragma: allowlist secret
	rotSealed     = "v1:1R76zdX0mH2rB3zFFC9SAiRhyuH7DwE4+VqJ46XgxIjomz54uhhxYHa2VBracHGtMtqe/1MsI4+aPq3pMvroAg1Fmsn+57aUREo=" // pragma: allowlist secret
)

// rotatingServer offers rotNewKey (sealed) to rotCurrentKey and records the
// key of every request.
func rotatingServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		out := map[string]any{"ok": true, "serverVersion": "test"}
		switch key {
		case rotCurrentKey:
			out["currentKeyId"] = 1
			out["pendingRotation"] = map[string]any{"keyId": 2, "apiKey": "", "sealedApiKey": rotSealed, "previousKeyExpiresAtUnix": 1900000000}
		case rotNewKey:
			out["currentKeyId"] = 2
		default:
			http.Error(w, "invalid or missing X-API-Key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func writeServerConfig(t *testing.T, baseURL, apiKeyValue string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "# operator notes\nserver:\n  baseUrl: \"" + baseURL + "\"\n  apiKey: " + apiKeyValue + "\nagentId: workstation-01\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewServerClientStoresRotatedKeyInConfig(t *testing.T) {
	srv, seen := rotatingServer(t)
	path := writeServerConfig(t, srv.URL, `"`+rotCurrentKey+`"`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	client := newServerClient(cfg, path)
	hs, err := client.Handshake(context.Background(), branchdam.HandshakeRequest{AgentID: cfg.AgentID})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if hs.CurrentKeyID != 2 {
		t.Fatalf("currentKeyId = %d, want 2 after adoption", hs.CurrentKeyID)
	}
	if got := seen(); len(got) != 2 || got[0] != rotCurrentKey || got[1] != rotNewKey {
		t.Fatalf("request keys = %d requests, want the current key then the rotated one", len(got))
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.APIKey != rotNewKey { // pragma: allowlist secret
		t.Fatal("config.yaml does not hold the rotated key")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# operator notes") {
		t.Errorf("comment lost:\n%s", raw)
	}
}

func TestNewServerClientLeavesEnvPlaceholder(t *testing.T) {
	srv, seen := rotatingServer(t)
	t.Setenv("BDA_TEST_ROTATION_KEY", rotCurrentKey)
	path := writeServerConfig(t, srv.URL, `"${BDA_TEST_ROTATION_KEY}"`)
	before, _ := os.ReadFile(path)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	client := newServerClient(cfg, path)
	hs, err := client.Handshake(context.Background(), branchdam.HandshakeRequest{AgentID: cfg.AgentID})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if hs.CurrentKeyID != 1 || hs.PendingRotation == nil {
		t.Fatalf("currentKeyId = %d: the client must not adopt a key it could not store", hs.CurrentKeyID)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatalf("config with a placeholder apiKey was rewritten:\n%s", after)
	}
	for _, k := range seen() {
		if k == rotNewKey {
			t.Fatal("rotated key was used without being stored")
		}
	}
}
