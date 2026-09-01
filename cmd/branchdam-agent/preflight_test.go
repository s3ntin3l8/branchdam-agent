package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

type fakeHelloCaller struct {
	resp *branchdam.HelloResponse
	err  error
}

func (f *fakeHelloCaller) Hello(_ context.Context) (*branchdam.HelloResponse, error) {
	return f.resp, f.err
}

func fakeLookPathFound(_ string) (string, error) { return "/usr/bin/exiftool", nil }
func fakeLookPathMissing(_ string) (string, error) {
	return "", errors.New("exec: \"exiftool\": executable file not found in $PATH")
}
func fakeRunVersionOK(_ string) (string, error) { return "13.10", nil }

func baseCfg() config.Config {
	return config.Config{
		Server: config.ServerConfig{
			BaseURL: "https://localhost:8080",
			APIKey:  "0123456789abcdef0123456789abcdef",
		},
	}
}

// TestRunPreflightChecksHappyPath is the regression test for the exact bug
// the plan flags as the likeliest silent failure: preflight must surface a
// non-empty server VERSION string, not just ok=true. A client that
// accidentally read HandshakeResponse's "serverVersion" field name against
// hello's actual "version" field would decode an empty string here and this
// test would catch it.
func TestRunPreflightChecksHappyPath(t *testing.T) {
	client := &fakeHelloCaller{resp: &branchdam.HelloResponse{OK: true, Version: "0.42.0"}}
	checks, ok := runPreflightChecks(context.Background(), baseCfg(), client, fakeLookPathFound, fakeRunVersionOK)
	if !ok {
		t.Fatalf("expected overall ok=true, checks: %v", checks)
	}
	found := false
	for _, c := range checks {
		if c.Status == "OK" && strings.Contains(c.Message, "version 0.42.0") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a check reporting server version 0.42.0, got: %v", checks)
	}
}

func TestRunPreflightChecksEmptyVersionIsFailure(t *testing.T) {
	// A 2xx response with OK=true but an empty Version must fail preflight
	// loudly rather than silently reporting success -- this is exactly what
	// would happen if HelloResponse and HandshakeResponse's field names
	// were ever accidentally unified.
	client := &fakeHelloCaller{resp: &branchdam.HelloResponse{OK: true, Version: ""}}
	checks, ok := runPreflightChecks(context.Background(), baseCfg(), client, fakeLookPathFound, fakeRunVersionOK)
	if ok {
		t.Fatalf("expected overall ok=false for an empty version, checks: %v", checks)
	}
	found := false
	for _, c := range checks {
		if c.Status == "FAIL" && strings.Contains(c.Message, "empty version") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a FAIL check mentioning empty version, got: %v", checks)
	}
}

func TestRunPreflightChecksServerUnreachable(t *testing.T) {
	client := &fakeHelloCaller{err: errors.New("dial tcp: connection refused")}
	checks, ok := runPreflightChecks(context.Background(), baseCfg(), client, fakeLookPathFound, fakeRunVersionOK)
	if ok {
		t.Fatalf("expected overall ok=false, checks: %v", checks)
	}
	if checks[0].Status != "FAIL" {
		t.Errorf("expected first check to FAIL, got %v", checks[0])
	}
}

func TestRunPreflightChecksMissingAPIKey(t *testing.T) {
	cfg := baseCfg()
	cfg.Server.APIKey = ""
	checks, ok := runPreflightChecks(context.Background(), cfg, nil, fakeLookPathFound, fakeRunVersionOK)
	if ok {
		t.Fatalf("expected overall ok=false for missing API key, checks: %v", checks)
	}
	if !strings.Contains(checks[0].Message, "apiKey is empty") {
		t.Errorf("expected apiKey-empty message, got %v", checks[0])
	}
}

// TestRunPreflightChecksUnexpandedAPIKeyPlaceholder is the regression test
// for the silent footgun config.Validate exists to catch: an unset ${VAR}
// in server.apiKey passes a naive `!= ""` check (it's the literal
// placeholder string, not empty) and would otherwise surface only as a
// confusing 503 from the server. preflight must report it as its own FAIL,
// not just fail check 1 with a dial/auth error that looks server-side.
func TestRunPreflightChecksUnexpandedAPIKeyPlaceholder(t *testing.T) {
	cfg := baseCfg()
	cfg.Server.APIKey = "${TEST_UNSET_VAR_XYZ}"
	client := &fakeHelloCaller{err: errors.New("dial tcp: connection refused")}
	checks, ok := runPreflightChecks(context.Background(), cfg, client, fakeLookPathFound, fakeRunVersionOK)
	if ok {
		t.Fatalf("expected overall ok=false, checks: %v", checks)
	}
	found := false
	for _, c := range checks {
		if c.Status == "FAIL" && strings.Contains(c.Message, "server.apiKey") && strings.Contains(c.Message, "unexpanded placeholder") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a FAIL check naming server.apiKey's unexpanded placeholder, got: %v", checks)
	}
}

func TestRunPreflightChecksExiftoolMissingIsWarnNotFail(t *testing.T) {
	client := &fakeHelloCaller{resp: &branchdam.HelloResponse{OK: true, Version: "0.42.0"}}
	checks, ok := runPreflightChecks(context.Background(), baseCfg(), client, fakeLookPathMissing, fakeRunVersionOK)
	if !ok {
		t.Fatalf("a missing exiftool must not fail preflight overall, checks: %v", checks)
	}
	found := false
	for _, c := range checks {
		if c.Status == "WARN" && strings.Contains(c.Message, "exiftool not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a WARN check about missing exiftool, got: %v", checks)
	}
}

func TestRunPreflightChecksPathMappingsPrinted(t *testing.T) {
	cfg := baseCfg()
	cfg.PathMappings = []config.PathMapping{
		{WorkstationPath: `D:\Photos`, ContainerPath: "/storage/archive"},
	}
	client := &fakeHelloCaller{resp: &branchdam.HelloResponse{OK: true, Version: "0.42.0"}}
	checks, ok := runPreflightChecks(context.Background(), cfg, client, fakeLookPathFound, fakeRunVersionOK)
	if !ok {
		t.Fatalf("expected ok=true, checks: %v", checks)
	}
	found := false
	for _, c := range checks {
		if strings.Contains(c.Message, `D:\Photos`) && strings.Contains(c.Message, "/storage/archive") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a check printing the configured path mapping, got: %v", checks)
	}
}

func TestPrintPreflightReportFormat(t *testing.T) {
	var buf bytes.Buffer
	checks := []preflightCheck{
		{"OK", "server reachable at http://localhost:8080, version 0.42.0"},
		{"WARN", "exiftool not found on PATH"},
	}
	printPreflightReport(&buf, "config.yaml", checks, true, 42*time.Millisecond)
	out := buf.String()
	if !strings.Contains(out, "version 0.42.0") {
		t.Errorf("report missing server version, got:\n%s", out)
	}
	if !strings.Contains(out, "preflight passed") {
		t.Errorf("report missing pass banner, got:\n%s", out)
	}
	if !strings.Contains(out, "config.yaml") {
		t.Errorf("report missing config path, got:\n%s", out)
	}
}

func TestPrintPreflightReportFailure(t *testing.T) {
	var buf bytes.Buffer
	checks := []preflightCheck{{"FAIL", "POST http://localhost:8080/api/v1/agent/hello: connection refused"}}
	printPreflightReport(&buf, "config.yaml", checks, false, time.Millisecond)
	if !strings.Contains(buf.String(), "preflight FAILED") {
		t.Errorf("report missing failure banner, got:\n%s", buf.String())
	}
}
