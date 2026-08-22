package branchdam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultTimeout is applied to a Client's underlying http.Client when none
// is supplied via WithHTTPClient.
const DefaultTimeout = 30 * time.Second

// Client is the branchDAM agent-server REST client. All four routes are
// POST-only (internal/httpapi/routes.go); Client never issues a GET against
// any of them.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client (e.g. to inject a
// shorter timeout or a custom transport in tests).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// New builds a Client for the branchDAM server at baseURL (no trailing
// slash required), authenticating with apiKey via the raw X-API-Key header
// (no scheme, unlike an Authorization: Bearer style header).
func New(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// post issues a POST to c.baseURL+path with body JSON-marshalled (or no
// body at all if body is nil, matching /hello's empty struct{} input), and
// JSON-unmarshals a 2xx response into out (skipped if out is nil). A
// non-2xx response is returned as *HTTPError with Body set to the raw
// response text -- 401/503 bodies are plain text, not JSON, so this never
// attempts to parse them as anything but a string.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("branchdam: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("branchdam: build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("branchdam: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("branchdam: read response %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(respBody))}
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("branchdam: decode response %s: %w", path, err)
	}
	return nil
}
