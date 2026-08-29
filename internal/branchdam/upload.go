package branchdam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// Upload streams raw media bytes directly to POST /api/v1/agent/upload.
// Returns an *UploadResponse containing the created NodeUUID, relativePath in the Master Archive,
// bytesWritten, and verified BLAKE3 hash.
func (c *Client) Upload(ctx context.Context, body io.Reader, opts UploadOptions) (*UploadResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/agent/upload", body)
	if err != nil {
		return nil, fmt.Errorf("branchdam: build upload request: %w", err)
	}

	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	if opts.Filename != "" {
		req.Header.Set("X-Filename", opts.Filename)
	}
	if opts.CameraModel != "" {
		req.Header.Set("X-Camera-Model", opts.CameraModel)
	}
	if opts.CaptureTimestamp > 0 {
		req.Header.Set("X-Capture-Timestamp", strconv.FormatInt(opts.CaptureTimestamp, 10))
	}
	if opts.Blake3Hash != "" {
		req.Header.Set("X-Blake3-Hash", opts.Blake3Hash)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("branchdam: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("branchdam: read upload response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(respBody))}
	}

	var out UploadResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("branchdam: decode upload response: %w", err)
	}
	return &out, nil
}
