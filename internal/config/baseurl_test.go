package config

import (
	"testing"
)

// TestValidateBaseURL exercises the BaseURL scheme/host policy added by
// issue #96: an absolute URL with scheme http or https is required, a
// trailing slash is rejected (it would concatenate to "host//api/..."),
// and cleartext http on a non-loopback host is refused outright.
func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		name        string
		baseURL     string
		wantField   string
		wantMessage string // substring required; empty means "any Problem on this field is enough"
	}{
		{
			name:      "trailing_slash_rejected",
			baseURL:   "http://localhost:8080/",
			wantField: "server.baseUrl",
		},
		{
			name:      "missing_scheme_rejected",
			baseURL:   "branchdam.example.com",
			wantField: "server.baseUrl",
		},
		{
			name:      "relative_path_rejected",
			baseURL:   "/api/v1/agent/hello",
			wantField: "server.baseUrl",
		},
		{
			name:      "http_non_loopback_refused",
			baseURL:   "http://10.0.0.1:8080",
			wantField: "server.baseUrl",
		},
		{
			name:      "https_remote_accepted",
			baseURL:   "https://branchdam.example.com",
			wantField: "",
		},
		{
			name:      "https_loopback_accepted",
			baseURL:   "https://127.0.0.1:8443",
			wantField: "",
		},
		{
			name:      "http_ipv6_loopback_warns",
			baseURL:   "http://[::1]:8080",
			wantField: "server.baseUrl",
		},
		{
			name:      "http_localhost_warns",
			baseURL:   "http://localhost:8080",
			wantField: "server.baseUrl",
		},
		{
			name:      "unsupported_scheme_rejected",
			baseURL:   "ftp://branchdam.example.com",
			wantField: "server.baseUrl",
		},
		{
			name:      "https_with_trailing_slash_rejected",
			baseURL:   "https://branchdam.example.com/",
			wantField: "server.baseUrl",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Server: ServerConfig{BaseURL: tc.baseURL}}
			problems := cfg.Validate()
			if tc.wantField == "" {
				for _, p := range problems {
					if p.Field == "server.baseUrl" {
						t.Errorf("expected no server.baseUrl problem for %q, got %v", tc.baseURL, problems)
					}
				}
				return
			}
			found := false
			for _, p := range problems {
				if p.Field == tc.wantField {
					found = true
				}
			}
			if !found {
				t.Errorf("expected a server.baseUrl problem for %q, got %v", tc.baseURL, problems)
			}
		})
	}
}

// TestValidateBaseURL_HTTPLoopbackWarns pins the spec's "warning" semantics:
// cleartext http on a loopback host must still surface a Problem (so an
// operator can see it in preflight/tray's startup-error surface) but must
// NOT be treated as the kind of error that prevents the agent from
// running. A single Problem on server.baseUrl with a message mentioning
// "cleartext" / "loopback" / "http" is what an operator can act on; the
// assertion is deliberately permissive on wording so the human-readable
// message can evolve without test churn.
func TestValidateBaseURL_HTTPLoopbackWarns(t *testing.T) {
	cfg := Config{Server: ServerConfig{BaseURL: "http://127.0.0.1:8080"}}
	problems := cfg.Validate()
	found := false
	for _, p := range problems {
		if p.Field == "server.baseUrl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a server.baseUrl warning for http://127.0.0.1:8080, got %v", problems)
	}
}

// TestValidateBaseURL_HTTPSNonLoopbackClean confirms https on a non-loopback
// host is the only fully-clean case: no server.baseUrl Problem at all.
// Catches a regression where an over-eager warning policy flags https too.
func TestValidateBaseURL_HTTPSNonLoopbackClean(t *testing.T) {
	cfg := Config{Server: ServerConfig{BaseURL: "https://branchdam.example.com"}}
	problems := cfg.Validate()
	for _, p := range problems {
		if p.Field == "server.baseUrl" {
			t.Errorf("expected no server.baseUrl problem for https://branchdam.example.com, got %v", problems)
		}
	}
}

// TestProblemAdvisory centralizes the "is this Problem advisory or
// blocking?" decision so call sites in cmd/branchdam-agent don't each
// write their own `p.Severity == SeverityWarning` comparison and silently
// drift when a future SeverityInfo / SeverityInfo tier is added. Today
// only SeverityWarning is advisory; zero-value Severity is the structural
// failure default.
func TestProblemAdvisory(t *testing.T) {
	cases := []struct {
		name     string
		problem  Problem
		advisory bool
	}{
		{"zero_value_severity_is_blocking", Problem{Field: "x", Message: "y"}, false},
		{"severity_warning_is_advisory", Problem{Field: "x", Message: "y", Severity: SeverityWarning}, true},
		{"empty_string_severity_is_blocking", Problem{Field: "x", Message: "y", Severity: ""}, false},
		{"unknown_severity_is_blocking", Problem{Field: "x", Message: "y", Severity: "info"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.problem.Advisory(); got != tc.advisory {
				t.Errorf("Advisory() = %v, want %v", got, tc.advisory)
			}
		})
	}
}
