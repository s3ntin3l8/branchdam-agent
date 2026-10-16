package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// TestTelemetryPassPostsToServer covers the tray's telemetry wiring end to
// end: registerTelemetry against a configured server, then one pass, must
// POST /api/v1/agent/telemetry with this agent's identity and the scratch
// root. SendTelemetry previously had no caller at all.
func TestTelemetryPassPostsToServer(t *testing.T) {
	var got atomic.Pointer[branchdam.AgentTelemetryInput]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/telemetry" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var in branchdam.AgentTelemetryInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got.Store(&in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"acknowledgedAtUnix":1}`))
	}))
	defer srv.Close()

	cfg := config.Config{AgentID: "ws-01"}
	cfg.Server.BaseURL = srv.URL
	cfg.Server.APIKey = "0123456789abcdef0123456789abcdef" // pragma: allowlist secret -- test fixture
	scratch := t.TempDir()
	runner := tray.NewRunner(nil, nil, scratch)
	client := branchdam.New(srv.URL, cfg.Server.APIKey)

	registerTelemetry(runner, client, cfg)
	runTelemetryPass(context.Background(), runner)

	in := got.Load()
	if in == nil {
		t.Fatal("no telemetry POST reached the server")
	}
	if in.AgentID != "ws-01" || in.ClientVersion != version || in.ScratchStorage.MountPath != scratch {
		t.Errorf("telemetry = %+v", *in)
	}

	// Paused: the pass must not reach the server.
	got.Store(nil)
	runner.SetPaused(true)
	runTelemetryPass(context.Background(), runner)
	if got.Load() != nil {
		t.Error("telemetry was sent while the tray was paused")
	}
}

func TestRegisterTelemetryClearsWhenServerNotConfigured(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	runner := tray.NewRunner(nil, nil, t.TempDir())
	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef") // pragma: allowlist secret -- test fixture
	registerTelemetry(runner, client, config.Config{})                   // no baseUrl/apiKey/agentId
	if ran, _ := runner.TriggerTelemetry(context.Background()); ran {
		t.Error("telemetry ran against an unconfigured server")
	}
	if hits.Load() != 0 {
		t.Error("the dummy client was dialed")
	}
}

// TestStartupTelemetryPassCarriesCapacity: the disk-usage cache is cold
// at startup and the server keeps only the latest snapshot, so the startup
// pass waits (bounded) for the first statfs reading instead of reporting
// zero capacity until the next tick.
func TestStartupTelemetryPassCarriesCapacity(t *testing.T) {
	var got atomic.Pointer[branchdam.AgentTelemetryInput]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in branchdam.AgentTelemetryInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		got.Store(&in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"acknowledgedAtUnix":1}`))
	}))
	defer srv.Close()

	cfg := config.Config{AgentID: "ws-01"}
	cfg.Server.BaseURL = srv.URL
	cfg.Server.APIKey = "0123456789abcdef0123456789abcdef" // pragma: allowlist secret -- test fixture
	runner := tray.NewRunner(nil, nil, t.TempDir())
	registerTelemetry(runner, branchdam.New(srv.URL, cfg.Server.APIKey), cfg)

	runStartupTelemetryPass(context.Background(), runner, startupUsageWait)

	in := got.Load()
	if in == nil {
		t.Fatal("the startup pass sent nothing")
	}
	if in.ScratchStorage.TotalBytes <= 0 {
		t.Errorf("startup snapshot TotalBytes = %d, want the scratch volume's capacity", in.ScratchStorage.TotalBytes)
	}
}
