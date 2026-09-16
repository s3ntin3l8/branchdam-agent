package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

func TestMapProbeErrorTranslates401(t *testing.T) {
	err := mapProbeError(&branchdam.HTTPError{StatusCode: 401, Body: "invalid or missing X-API-Key"})
	if err == nil || err.Error() != "server rejected the agent API key" {
		t.Errorf("got %v, want the curated 401 message", err)
	}
}

func TestMapProbeErrorTranslates503(t *testing.T) {
	err := mapProbeError(&branchdam.HTTPError{StatusCode: 503, Body: "agent authentication is not configured"})
	want := "server rejected the agent API key — it must be at least 32 characters"
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

// TestMapProbeErrorNeverLeaksBody pins the audit S-7 constraint
// HTTPError.Error() itself already enforces (errors.go's own doc
// comment): the curated 401/503 messages must never echo the response
// Body, which can reflect the request payload or name the X-API-Key.
func TestMapProbeErrorNeverLeaksBody(t *testing.T) {
	secret := "super-secret-api-key-value-that-must-never-leak" // pragma: allowlist secret -- test fixture, not a real credential
	for _, code := range []int{401, 503} {
		err := mapProbeError(&branchdam.HTTPError{StatusCode: code, Body: secret})
		if strings.Contains(err.Error(), secret) {
			t.Errorf("status %d: mapped error %q leaks the response body", code, err.Error())
		}
	}
}

func TestMapProbeErrorPassesThroughOtherStatusCodes(t *testing.T) {
	orig := &branchdam.HTTPError{StatusCode: 500, Body: "internal error"}
	err := mapProbeError(orig)
	if err != orig {
		t.Errorf("got %v, want the original error passed through unchanged for a non-401/503 status", err)
	}
}

func TestMapProbeErrorPassesThroughNonHTTPErrors(t *testing.T) {
	orig := context.DeadlineExceeded
	if err := mapProbeError(orig); err != orig {
		t.Errorf("got %v, want the original non-HTTPError passed through unchanged", err)
	}
}

func TestHelloProbeReturnsVersionOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"1.10.0"}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	p := &helloProbe{client: client}
	version, err := p.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if version != "1.10.0" {
		t.Errorf("version = %q, want 1.10.0", version)
	}
}

// TestHelloProbeRejectsEmptyVersion mirrors preflight.go's own guard: a
// 2xx response with an empty version string signals a hello/handshake
// field-name mixup (hello returns "version", handshake returns
// "serverVersion"), not a healthy server -- it must not read as reachable.
func TestHelloProbeRejectsEmptyVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":""}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	p := &helloProbe{client: client}
	if _, err := p.Probe(context.Background()); err == nil {
		t.Error("expected Probe to error on an empty version, got nil")
	}
}

func TestHelloProbeMapsAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid or missing X-API-Key"))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	p := &helloProbe{client: client}
	_, err := p.Probe(context.Background())
	if err == nil || err.Error() != "server rejected the agent API key" {
		t.Errorf("got %v, want the curated 401 message", err)
	}
}

func TestRegisterServerProbeClearsWhenNotConfigured(t *testing.T) {
	runner := tray.NewRunner(noopIngester{}, nil, "")
	registerServerProbe(runner, branchdam.New("http://localhost:1", ""), false)

	_, ran := runner.TriggerServerProbe(context.Background())
	if ran {
		t.Error("expected TriggerServerProbe to report ran=false when registerServerProbe was called with configured=false")
	}
}

func TestRegisterServerProbeWiresRealClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"1.10.0"}`))
	}))
	defer srv.Close()

	runner := tray.NewRunner(noopIngester{}, nil, "")
	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	registerServerProbe(runner, client, true)

	result, ran := runner.TriggerServerProbe(context.Background())
	if !ran {
		t.Fatal("expected TriggerServerProbe to run once registerServerProbe wired a real client")
	}
	if !result.OK || result.Version != "1.10.0" {
		t.Errorf("got %+v, want OK=true Version=1.10.0", result)
	}
}
