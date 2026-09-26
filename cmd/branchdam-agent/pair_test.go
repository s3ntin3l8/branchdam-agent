package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// TestPairCmdPersistsCredentials covers the happy path: a stub server
// accepts the presented key, the agent validates via Hello(), and the
// CLI persists server.baseUrl + server.apiKey + agentId into the
// config file atomically. Asserts the post-state on disk (yaml parsed,
// not just bytes) so a regression in config.Patch's interaction with
// the file mode / atom-rename would surface here.
func TestPairCmdPersistsCredentials(t *testing.T) {
	const (
		wantKey   = "01234567890123456789012345678901" // 32 chars
		wantAgent = "dev-d2610219"
	)

	// Stub server: only /api/v1/agent/hello is exercised by the CLI's
	// pre-write validation. The token round-trip is the only contract
	// the CLI depends on; /api/v1/agent/hello is auth-gated by the same
	// X-API-Key path that a real Companion-Pairing-minted key uses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/hello" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("X-API-Key"); got != wantKey {
			http.Error(w, "wrong key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(branchdam.HelloResponse{OK: true, Version: "v0.22.0"})
	}))
	defer srv.Close()
	wantURL := srv.URL

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Minimal pre-existing config so the test exercises Patch's
	// "edit-in-place" path, not the "create empty doc" path.
	initial := []byte("server:\n  baseUrl: \"\"\n  apiKey: \"\"\nagentId: \"\"\n")
	if err := os.WriteFile(cfgPath, initial, 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// On Windows, refuseIfPermsLeaky is a no-op (POSIX bits aren't
	// exposed), and os.Chmod(0o600) would set the read-only attribute
	// -- which would then make the subsequent config.Patch fail. Only
	// chmod on POSIX where the guard actually runs.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cfgPath, 0o600); err != nil {
			t.Fatalf("chmod seed config: %v", err)
		}
	}

	rawURL := "branchdam://?server=" + urlEscape(t, wantURL) + "&key=" + wantKey + "&agent=" + wantAgent
	rc := runPairCmd([]string{"-config", cfgPath, rawURL})
	if rc != 0 {
		t.Fatalf("runPairCmd rc = %d, want 0", rc)
	}

	// Re-parse the config to confirm the patch landed correctly. The CLI
	// doesn't print the persisted contents (it prints a "paired with X"
	// summary), so a regression that silently dropped a field would
	// only show up here.
	var got struct {
		Server struct {
			BaseURL string `yaml:"baseUrl"`
			APIKey  string `yaml:"apiKey"`
		} `yaml:"server"`
		AgentID string `yaml:"agentId"`
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if got.Server.BaseURL != wantURL {
		t.Errorf("server.baseUrl = %q, want %q", got.Server.BaseURL, wantURL)
	}
	if got.Server.APIKey != wantKey {
		t.Errorf("server.apiKey = %q, want %q", got.Server.APIKey, wantKey)
	}
	if got.AgentID != wantAgent {
		t.Errorf("agentId = %q, want %q", got.AgentID, wantAgent)
	}

	// Mode must be 0600 -- config.Patch promises this, but a regression
	// in the atomic-rename path that widened perms would put a plaintext
	// API key into a leaky file. Verify directly via os.Stat. Skip on
	// Windows where the perms-translation gap means FileInfo.Mode().Perm()
	// always reports 0666 regardless of what config.Patch wrote; ACLs
	// are the right surface there, and config.Patch's atomic temp+rename
	// still ran (the post-rename file is reachable, owned by the same
	// user, and writable).
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("config mode = %#o, want 0600", mode)
		}
	}
}

// TestPairCmdRefusesLeakyExistingConfig verifies the pre-write guard
// against writing a plaintext API key into a world- or group-readable
// config -- a regression here would silently embed a secret in a file
// any process on the host can read. The CLI must refuse without
// touching the file.
//
// Skip on Windows: per the same rationale internal/config.Load's
// checkFilePermissions documents, Windows exposes ACLs rather than
// POSIX group/world mode bits, so refuseIfPermsLeaky is a no-op there
// and the leaky-config rejection path can't be exercised. The "embed a
// secret" risk doesn't apply on Windows the same way -- ACLs are the
// right surface, and this CLI doesn't own ACL management.
func TestPairCmdRefusesLeakyExistingConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows exposes ACLs rather than POSIX group/world mode bits; refuseIfPermsLeaky is a no-op there")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(branchdam.HelloResponse{OK: true})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server: {}\n"), 0o644); err != nil { // world-readable
		t.Fatalf("seed config: %v", err)
	}
	originalBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	rawURL := "branchdam://?server=" + urlEscape(t, srv.URL) + "&key=01234567890123456789012345678901&agent=dev-x"
	rc := runPairCmd([]string{"-config", cfgPath, rawURL})
	if rc == 0 {
		t.Fatalf("runPairCmd rc = 0, want non-zero (must refuse leaky config)")
	}

	// File must be unchanged -- no partial write of even non-secret
	// fields, since refusing the write is the only correct behavior.
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(originalBytes) {
		t.Errorf("config was modified despite refusal; before=%q after=%q", originalBytes, after)
	}
}

// TestPairCmdRejectsServerRefusal verifies a pasted key the server
// rejects does not silently overwrite the existing config -- a
// regression here would let an operator paste a stale or revoked key
// and lose their working credentials.
func TestPairCmdRejectsServerRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "revoked", http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := []byte("server:\n  baseUrl: \"https://old.example.com\"\n  apiKey: \"old-working-key-that-must-survive\"\nagentId: \"dev-old\"\n")
	if err := os.WriteFile(cfgPath, initial, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Same Windows port as the happy-path test: skip the chmod on
	// Windows where it would set the read-only attribute and break the
	// server-refusal path we're trying to exercise.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(cfgPath, 0o600); err != nil {
			t.Fatalf("chmod seed: %v", err)
		}
	}
	originalBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	rawURL := "branchdam://?server=" + urlEscape(t, srv.URL) + "&key=01234567890123456789012345678901&agent=dev-new"
	rc := runPairCmd([]string{"-config", cfgPath, rawURL})
	if rc == 0 {
		t.Fatalf("runPairCmd rc = 0, want non-zero (server rejected the key)")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(originalBytes) {
		t.Errorf("config was modified despite server refusal")
	}
	if !strings.Contains(strings.ToLower(string(after)), "old-working-key") {
		t.Errorf("working key was lost")
	}
}

// TestPairCmdRequiresURL covers the usage-error path: a `pair` invocation
// without a positional branchdam:// URL must exit with a usage error and
// not touch the config.
func TestPairCmdRequiresURL(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server: {}\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rc := runPairCmd([]string{"-config", cfgPath})
	if rc == 0 {
		t.Fatalf("rc = 0, want non-zero (no URL given)")
	}
}

func TestPairCmdRejectsInvalidConfigBeforeWriting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(branchdam.HelloResponse{OK: true})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	initial := []byte("server:\n  baseUrl: \"http://127.0.0.1:8080\"\n  apiKey: \"initial-key-0123456789012345678\"\nagentId: \"dev-initial\"\n")
	if err := os.WriteFile(cfgPath, initial, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Trailing slash in server URL fails checkServerBaseURL / Validate()
	rawURL := "branchdam://?server=" + urlEscape(t, srv.URL+"/") + "&key=01234567890123456789012345678901&agent=dev-new"
	rc := runPairCmd([]string{"-config", cfgPath, rawURL})
	if rc == 0 {
		t.Fatalf("runPairCmd rc = 0, want non-zero (trailing slash must be rejected by pre-validation)")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(initial) {
		t.Errorf("config was modified despite pre-validation failure; before=%q after=%q", initial, after)
	}
}

// urlEscape is a thin wrapper around url.QueryEscape so the test doesn't
// pull in net/url just for the escape call (cmd/branchdam-agent/pair.go
// uses ParsePairingURL which already accepts url-encoded values, so the
// test inputs are pre-encoded to match what an operator would paste).
func urlEscape(t *testing.T, s string) string {
	t.Helper()
	const hexChars = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexChars[c>>4])
		b.WriteByte(hexChars[c&0x0F])
	}
	return b.String()
}

// TestPairCmdCreatesMissingConfig covers the deep-link-on-a-fresh-machine
// case: no config.yaml (nor its directory) exists, and pair must create it
// rather than fail on the Load/Patch read.
func TestPairCmdCreatesMissingConfig(t *testing.T) {
	const (
		wantKey   = "01234567890123456789012345678901" // 32 chars
		wantAgent = "dev-fresh"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/hello" || r.Header.Get("X-API-Key") != wantKey {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(branchdam.HelloResponse{OK: true, Version: "v0.22.0"})
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "fresh", "config.yaml")
	rawURL := "branchdam://?server=" + urlEscape(t, srv.URL) + "&key=" + wantKey + "&agent=" + wantAgent
	if rc := runPairCmd([]string{"-config", cfgPath, rawURL}); rc != 0 {
		t.Fatalf("runPairCmd rc = %d, want 0", rc)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load created config: %v", err)
	}
	if cfg.Server.BaseURL != srv.URL || cfg.Server.APIKey != wantKey || cfg.AgentID != wantAgent { // pragma: allowlist secret
		t.Errorf("credentials not persisted: baseUrl=%q agentId=%q", cfg.Server.BaseURL, cfg.AgentID)
	}
	if p := firstBlockingProblem(cfg); p != nil {
		t.Errorf("created config has blocking problem: %s", p)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("stat/mode = %v, %v; want 0600", fi, err)
		}
	}
}

// A server reply means the key was rejected; a dial failure means the server
// was never reached and must not be described as a rejection.
func TestPairConfigHelloFailureWording(t *testing.T) {
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "revoked", http.StatusUnauthorized)
	}))
	defer rejecting.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listening: connection refused

	cfg := config.Config{}
	for _, tc := range []struct {
		name, server, want, notWant string
	}{
		{"server replied 401", rejecting.URL, "rejected the key", "could not reach"},
		{"server unreachable", deadURL, "could not reach server at " + deadURL, "rejected the key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pairConfig(filepath.Join(t.TempDir(), "config.yaml"), tc.server, "01234567890123456789012345678901", "dev-x", 5*time.Second, cfg)
			if err == nil {
				t.Fatal("want an error")
			}
			if !errors.Is(err, branchdam.ErrPairingHelloFailed) {
				t.Errorf("err %v must still wrap ErrPairingHelloFailed", err)
			}
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("err = %q, want it to contain %q and not %q", err, tc.want, tc.notWant)
			}
		})
	}
}

func TestLocalNetworkHint(t *testing.T) {
	ehostunreach := fmt.Errorf("dial tcp 192.168.2.204:443: %w", syscall.EHOSTUNREACH)
	if got := localNetworkHint("darwin", ehostunreach); !strings.Contains(got, "Local Network") {
		t.Errorf("darwin + EHOSTUNREACH: hint = %q, want the Local Network pointer", got)
	}
	if got := localNetworkHint("linux", ehostunreach); got != "" {
		t.Errorf("hint off darwin = %q, want none", got)
	}
	if got := localNetworkHint("darwin", errors.New("x509: certificate signed by unknown authority")); got != "" {
		t.Errorf("hint for an unrelated error = %q, want none", got)
	}
}
