package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRunUnknownSubcommand(t *testing.T) {
	if got := run([]string{"bogus"}); got != 2 {
		t.Errorf("run([bogus]) = %d, want 2", got)
	}
}

func TestRunNoArgs(t *testing.T) {
	if got := run(nil); got != 2 {
		t.Errorf("run(nil) = %d, want 2", got)
	}
}

func TestRunHelp(t *testing.T) {
	if got := run([]string{"help"}); got != 0 {
		t.Errorf("run([help]) = %d, want 0", got)
	}
}

func TestRunVersion(t *testing.T) {
	if got := run([]string{"version"}); got != 0 {
		t.Errorf("run([version]) = %d, want 0", got)
	}
}

// TestRunPreflightAgainstRealHTTPServer exercises the full stack --
// config.Load, branchdam.New, branchdam.Client.Hello over real HTTP -- not
// just the fake helloCaller preflight_test.go uses, so a wiring bug between
// main.go and internal/branchdam (wrong header name, wrong path, wrong
// method) would fail here even if the unit-level fakes couldn't catch it.
func TestRunPreflightAgainstRealHTTPServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/hello", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != "0123456789abcdef0123456789abcdef" {
			t.Errorf("X-API-Key = %q, want the configured key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"0.99.9-test"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "server:\n  baseUrl: \"" + srv.URL + "\"\n  apiKey: \"0123456789abcdef0123456789abcdef\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"preflight", "-config", cfgPath, "-timeout", "5s"})
	if got != 0 {
		t.Errorf("run([preflight]) against a real hello server = %d, want 0", got)
	}
}
