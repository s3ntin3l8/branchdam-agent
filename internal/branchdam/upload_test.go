package branchdam

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientUpload(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotFilename, gotCamera, gotTimestamp, gotHash, gotContentType string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("X-API-Key")
		gotFilename = r.Header.Get("X-Filename")
		gotCamera = r.Header.Get("X-Camera-Model")
		gotTimestamp = r.Header.Get("X-Capture-Timestamp")
		gotHash = r.Header.Get("X-Blake3-Hash")
		gotContentType = r.Header.Get("Content-Type")

		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"nodeUuid": "018f2345-6789-7abc-def0-123456789abc",
			"status": "UPLOADED",
			"bytesWritten": 12,
			"blake3Hash": "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a",
			"relativePath": "2026/2026-08-29_Pixel-9-Pro/IMG_0001.JPG"
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-api-key-32-chars-minimum-key")
	opts := UploadOptions{
		Filename:         "IMG_0001.JPG",
		CameraModel:      "Pixel-9-Pro",
		CaptureTimestamp: 1756470000,
		Blake3Hash:       "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a",
	}

	resp, err := c.Upload(context.Background(), strings.NewReader("hello upload"), opts)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/agent/upload" {
		t.Errorf("path = %q, want /api/v1/agent/upload", gotPath)
	}
	if gotAPIKey != "test-api-key-32-chars-minimum-key" {
		t.Errorf("apiKey = %q", gotAPIKey)
	}
	if gotFilename != "IMG_0001.JPG" {
		t.Errorf("filename = %q", gotFilename)
	}
	if gotCamera != "Pixel-9-Pro" {
		t.Errorf("camera = %q", gotCamera)
	}
	if gotTimestamp != "1756470000" {
		t.Errorf("timestamp = %q", gotTimestamp)
	}
	if gotHash != "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a" {
		t.Errorf("hash = %q", gotHash)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("contentType = %q", gotContentType)
	}
	if string(gotBody) != "hello upload" {
		t.Errorf("body = %q, want 'hello upload'", string(gotBody))
	}

	if resp.NodeUUID != "018f2345-6789-7abc-def0-123456789abc" {
		t.Errorf("resp.NodeUUID = %q", resp.NodeUUID)
	}
	if resp.Status != "UPLOADED" {
		t.Errorf("resp.Status = %q", resp.Status)
	}
	if resp.BytesWritten != 12 {
		t.Errorf("resp.BytesWritten = %d", resp.BytesWritten)
	}
	if resp.RelativePath != "2026/2026-08-29_Pixel-9-Pro/IMG_0001.JPG" {
		t.Errorf("resp.RelativePath = %q", resp.RelativePath)
	}
}

func TestClientUpload_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"missing X-Filename header"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-api-key")
	_, err := c.Upload(context.Background(), strings.NewReader(""), UploadOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	httpErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", httpErr.StatusCode)
	}
}

func TestHandshake_WithNamingTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"ok": true,
			"serverVersion": "0.12.0",
			"serverTimeUnix": 1756470000,
			"namingTemplate": "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}"
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key")
	resp, err := c.Handshake(context.Background(), HandshakeRequest{AgentID: "agent-01"})
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if resp.NamingTemplate != "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}" {
		t.Errorf("NamingTemplate = %q", resp.NamingTemplate)
	}
}
