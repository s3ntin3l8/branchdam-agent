package branchdam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSendTelemetry(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotContentType string
	var gotBody AgentTelemetryInput

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-API-Key")
		gotContentType = r.Header.Get("Content-Type")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("unmarshal telemetry input: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true, "acknowledgedAtUnix": 1724846401}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-api-key-32-chars-minimum-key") // pragma: allowlist secret
	in := AgentTelemetryInput{
		AgentID:       "workstation-macbook-01",
		ClientVersion: "1.1.0",
		TimestampUnix: 1724846400,
		ScratchStorage: AgentScratchStorageDTO{
			MountPath:            "/Volumes/Scratch",
			TotalBytes:           2000000000000,
			FreeBytes:            500000000000,
			UsedBytes:            1500000000000,
			MirrorsSizeBytes:     300000000000,
			RenderCacheSizeBytes: 800000000000,
			ProxiesSizeBytes:     200000000000,
			PrunableBytes:        400000000000,
		},
		PruneStats: &AgentPruneStatsDTO{
			LastPruneTimestampUnix: 1724842800,
			LastReclaimedBytes:     100000000000,
			LastPruneDurationMs:    1500,
			PrunedItemCounts: map[string]int{
				"mirrors": 5,
			},
		},
	}

	out, err := c.SendTelemetry(context.Background(), in)
	if err != nil {
		t.Fatalf("SendTelemetry: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/agent/telemetry" {
		t.Errorf("path = %q, want /api/v1/agent/telemetry", gotPath)
	}
	if gotAPIKey != "test-api-key-32-chars-minimum-key" { // pragma: allowlist secret
		t.Errorf("X-API-Key = %q", gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody.AgentID != "workstation-macbook-01" {
		t.Errorf("gotBody.AgentID = %q", gotBody.AgentID)
	}
	if gotBody.ScratchStorage.MountPath != "/Volumes/Scratch" {
		t.Errorf("mountPath = %q", gotBody.ScratchStorage.MountPath)
	}
	if !out.OK || out.AcknowledgedAtUnix != 1724846401 {
		t.Errorf("unexpected output: %+v", out)
	}
}

func TestClientSendTelemetryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "bad-key")
	_, err := c.SendTelemetry(context.Background(), AgentTelemetryInput{AgentID: "test"})
	if err == nil {
		t.Fatal("expected error on 403 Forbidden, got nil")
	}
}
