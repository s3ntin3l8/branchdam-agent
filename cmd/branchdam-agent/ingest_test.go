package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestRunIngestAgainstRealHTTPServer exercises the full ingest stack --
// config.Load, branchdam.New, ingest.Engine, and a real POST
// /api/v1/agent/events -- against a fixture card containing one file, the
// same "wiring bug would fail here" rationale as
// TestRunPreflightAgainstRealHTTPServer above.
func TestRunIngestAgainstRealHTTPServer(t *testing.T) {
	var mu sync.Mutex
	var envelopes []map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		var env map[string]any
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		mu.Lock()
		envelopes = append(envelopes, env)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), []byte("fixture-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: \"" + archiveRoot + "\"\n" +
		"    containerPath: \"/storage/archive\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + archiveRoot + "\"\n" +
		"  localEditRoot: \"" + localRoot + "\"\n" +
		"  pathTemplate: \"{original_name}\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"ingest", "-config", cfgPath, "-card", cardRoot, "-timeout", "30s"})
	if got != 0 {
		t.Fatalf("run([ingest]) = %d, want 0", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(envelopes) != 1 {
		t.Fatalf("got %d event envelopes, want 1", len(envelopes))
	}
	env := envelopes[0]
	if env["agentId"] != "test-agent" {
		t.Errorf("agentId = %v, want test-agent", env["agentId"])
	}
	if env["eventType"] != "EVENT_NODE_CREATED" {
		t.Errorf("eventType = %v, want EVENT_NODE_CREATED", env["eventType"])
	}
	payloadStr, ok := env["payload"].(string)
	if !ok {
		t.Fatalf("payload is not a JSON string (double-encoding violated): %T", env["payload"])
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		t.Fatalf("payload does not parse as JSON: %v", err)
	}
	if payload["filePath"] != "/storage/archive/IMG_0001.jpg" {
		t.Errorf("filePath = %v, want /storage/archive/IMG_0001.jpg", payload["filePath"])
	}

	if _, err := os.Stat(filepath.Join(archiveRoot, "IMG_0001.jpg")); err != nil {
		t.Errorf("archive destination missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "IMG_0001.jpg")); err != nil {
		t.Errorf("local destination missing: %v", err)
	}
}

func TestRunIngestMissingCardFlag(t *testing.T) {
	if got := run([]string{"ingest", "-config", "config.yaml"}); got != 2 {
		t.Errorf("run([ingest] without -card) = %d, want 2", got)
	}
}

func TestRunIngestCollisionMultiSubdir(t *testing.T) {
	var mu sync.Mutex
	var envelopes []map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		_ = json.NewDecoder(r.Body).Decode(&env)
		mu.Lock()
		envelopes = append(envelopes, env)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	sub1 := filepath.Join(cardRoot, "DCIM", "100")
	sub2 := filepath.Join(cardRoot, "DCIM", "101")
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	if err := os.MkdirAll(sub1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub1, "DSC0001.JPG"), []byte("photo-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub2, "DSC0001.JPG"), []byte("photo-2"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: \"" + archiveRoot + "\"\n" +
		"    containerPath: \"/storage/archive\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + archiveRoot + "\"\n" +
		"  localEditRoot: \"" + localRoot + "\"\n" +
		"  pathTemplate: \"{original_name}\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"ingest", "-config", cfgPath, "-card", cardRoot, "-timeout", "30s"})
	if got != 0 {
		t.Fatalf("run([ingest]) = %d, want 0", got)
	}

	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001.JPG")); err != nil {
		t.Errorf("expected DSC0001.JPG in archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001_2.JPG")); err != nil {
		t.Errorf("expected DSC0001_2.JPG in archive: %v", err)
	}
}

func TestRunIngestUploadStreaming(t *testing.T) {
	var mu sync.Mutex
	var uploadCount int
	var handshakeCalled bool

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/handshake", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handshakeCalled = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"ok": true,
			"serverVersion": "0.12.0",
			"serverTimeUnix": 1756470000,
			"namingTemplate": "{yyyy}/{original_name}"
		}`))
	})
	mux.HandleFunc("/api/v1/agent/upload", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uploadCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"nodeUuid": "018f-upload-uuid",
			"status": "UPLOADED",
			"bytesWritten": 12,
			"blake3Hash": "c69ee721fa225ae783156722ee0d67961d9f8c47b0715f3beda16fef9e82b3cf",
			"relativePath": "2026/PXL_0001.jpg"
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "PXL_0001.jpg"), []byte("sample-photo"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  localEditRoot: \"" + localRoot + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{"ingest", "-config", cfgPath, "-card", cardRoot, "-upload", "-timeout", "30s"})
	if got != 0 {
		t.Fatalf("run([ingest -upload]) = %d, want 0", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if !handshakeCalled {
		t.Error("expected handshake to be called")
	}
	if uploadCount != 1 {
		t.Errorf("uploadCount = %d, want 1", uploadCount)
	}

	// Local file should be created under localRoot / 2026 / PXL_0001.jpg
	localFile := filepath.Join(localRoot, "2026", "PXL_0001.jpg")
	data, err := os.ReadFile(localFile)
	if err != nil {
		t.Fatalf("read local file: %v", err)
	}
	if string(data) != "sample-photo" {
		t.Errorf("local file data = %q, want 'sample-photo'", string(data))
	}
}
