package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// yamlQuote returns a JSON string literal, which is also a valid YAML quoted
// scalar. Unlike hand-built quotes it escapes Windows path backslashes.
func yamlQuote(s string) string { return strconv.Quote(s) }

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

func TestEffectiveLaunchArgs(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		executable string
		args       []string
		want       []string
	}{
		{name: "windows tray", goos: "windows", executable: `C:\\Program Files\\branchDAM\\branchdam-agent-tray.exe`, want: []string{"tray"}},
		{name: "windows tray case insensitive", goos: "windows", executable: `C:\\branchdam-agent-TRAY.EXE`, want: []string{"tray"}},
		{name: "windows console", goos: "windows", executable: `C:\\branchdam-agent.exe`},
		{name: "mac bundle", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", want: []string{"tray"}},
		{name: "mac console", goos: "darwin", executable: "/usr/local/bin/branchdam-agent"},
		{name: "linux", goos: "linux", executable: "/usr/local/bin/branchdam-agent"},
		{name: "explicit args win", goos: "windows", executable: `C:\\branchdam-agent-tray.exe`, args: []string{"version"}, want: []string{"version"}},
		{name: "explicit args mac", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"preflight"}, want: []string{"preflight"}},
		{name: "deep link url arg", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"branchdam://?server=http://localhost&key=123"}, want: []string{"pair", "-deeplink", "branchdam://?server=http://localhost&key=123"}},
		{name: "deep link with mac os launch args", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"-psn_0_123456", "branchdam://?server=http://localhost&key=123"}, want: []string{"pair", "-deeplink", "branchdam://?server=http://localhost&key=123"}},
		// Beyond a bare OS handoff, nothing is rewritten: an explicit
		// subcommand (even "pair") keeps its own argv so its FlagSet sees
		// every flag, and a stray URL can never hijack another command.
		{name: "explicit pair with config flag", goos: "linux", executable: "/usr/local/bin/branchdam-agent", args: []string{"pair", "-config", "/tmp/cd/config.yaml", "branchdam://?server=http://localhost&key=123"}, want: []string{"pair", "-config", "/tmp/cd/config.yaml", "branchdam://?server=http://localhost&key=123"}},
		{name: "stray url does not hijack tray", goos: "linux", executable: "/usr/local/bin/branchdam-agent", args: []string{"tray", "branchdam://?server=http://localhost&key=123"}, want: []string{"tray", "branchdam://?server=http://localhost&key=123"}},
		{name: "two deep link urls not rewritten", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"branchdam://a", "branchdam://b"}, want: []string{"branchdam://a", "branchdam://b"}},
		{name: "unknown flag alongside url not rewritten", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"--verbose", "branchdam://?server=http://localhost&key=123"}, want: []string{"--verbose", "branchdam://?server=http://localhost&key=123"}},
		{name: "psn noise without url not rewritten", goos: "darwin", executable: "/Applications/branchdam-agent.app/Contents/MacOS/branchdam-agent", args: []string{"-psn_0_123456"}, want: []string{"-psn_0_123456"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveLaunchArgs(tt.goos, tt.executable, tt.args)
			if len(got) != len(tt.want) {
				t.Fatalf("effectiveLaunchArgs() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("effectiveLaunchArgs() = %v, want %v", got, tt.want)
				}
			}
		})
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
