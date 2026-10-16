package branchdam

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout is applied to a Client's underlying http.Client when none
// is supplied via WithHTTPClient. It bounds every JSON call; Upload uses a
// separate client without a total timeout (see upload.go).
const DefaultTimeout = 30 * time.Second

// Client is the branchDAM agent-server REST client.
//
// The API key can change during the client's life: Handshake adopts a
// rotated key the server delivers (keyrotation.go), so every request reads
// the current key once, under keyMu, and signs and authenticates with that
// one value.
type Client struct {
	baseURL    string
	httpClient *http.Client

	// uploadClient carries Upload only. It has no total Timeout -- see
	// upload.go's uploadTransport for the per-phase deadlines and idle
	// watchdog that replace it.
	uploadClient      *http.Client
	uploadIdleTimeout time.Duration

	persistKey KeyPersister

	// adoptMu serializes rotated-key adoption (adoptRotation).
	adoptMu sync.Mutex

	// keyMu guards every field below.
	keyMu  sync.Mutex
	apiKey string
	// adopting is non-nil from the switch to a rotated key until the
	// server first answers a request made with it.
	adopting *keyAdoption
	// restoreKey is the previous key after the server refused the rotated
	// one; it is written back to the key store once the server accepts it.
	restoreKey string
	// refusedKeyIDs are rotated key ids the server refused on first use;
	// they are not adopted again in this process.
	refusedKeyIDs map[int64]struct{}
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client (e.g. to inject a
// shorter timeout or a custom transport in tests).
//
// SECURITY: this option replaces the entire *http.Client wholesale,
// including the http.Transport that secureTransport() constructs with a
// TLS 1.2 floor (S-2). A caller that supplies a *http.Client with
// Transport == nil falls back to http.DefaultTransport -- Go's default,
// which permits TLS 1.0/1.1 -- silently downgrading that protection. The
// same applies to a caller that constructs its own Transport without
// setting TLSClientConfig.MinVersion, which is exactly the field
// TestClientUsesTLS12MinimumTransport inspects on the default. Production
// call sites should not pass this option; it is intended for tests and
// power-users who explicitly need to take ownership of the wire layer.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// secureTransport is the http.Transport every Client builds by default --
// TLS 1.2 floor is the S-2 defense-in-depth layer. internal/config.Validate
// already rejects cleartext http:// on a non-loopback host at config-load
// time (issue #96), and NewClient panics if a caller bypasses that check
// with a hard-coded URL -- so a Transport-level downgrade here would only
// matter for an HTTPS URL whose handshake negotiates below TLS 1.2. Go's
// default Transport accepts TLS 1.0/1.1, so pinning the floor matters.
func secureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

// New builds a Client for the branchDAM server at baseURL (no trailing
// slash required), authenticating with apiKey via the raw X-API-Key header
// (no scheme, unlike an Authorization: Bearer style header).
//
// baseURL must be https://, or http:// to a loopback host -- anything
// else panics. internal/config.Validate already rejects this same case at
// config-load time (issue #96); the panic here is the defense-in-depth
// layer that catches a caller who bypasses Validate (a test, a future
// subcommand, an SDK consumer). The X-API-Key shared secret must never
// be sent over a cleartext wire.
func New(baseURL, apiKey string, opts ...Option) *Client {
	if err := validateBaseURL(baseURL); err != nil {
		panic(fmt.Sprintf("branchdam: refusing to construct Client: %v", err))
	}
	c := &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout:   DefaultTimeout,
			Transport: secureTransport(),
		},
		uploadClient:      newUploadHTTPClient(),
		uploadIdleTimeout: DefaultUploadIdleTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ValidateServerURL is the parse-and-refuse gate that both New's
// panic-on-programmer-error path and ParsePairingURL's
// typed-error-on-user-input path consult. It is intentionally a thin
// wrapper over net/url's Parse + a single allow/deny rule -- the
// exhaustive BaseURL policy (trailing slash, scheme allowlist,
// loopback carve-out) lives in internal/config.checkServerBaseURL and
// runs on every Load; this gate only repeats the cleartext-non-loopback
// half, which is the one an attacker can weaponize even when a process
// skips Load entirely (e.g. an embedded test harness) or pastes a
// pairing URL with a malformed server field (the `pair` CLI).
//
// Exported so ParsePairingURL can return a typed ErrPairingURLInvalid
// rather than panicking through New's contract; New itself still
// panics on a non-nil result because a Client built directly with a bad
// URL is a programmer error, not a user-input condition.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("server.baseUrl %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("server.baseUrl %q has scheme %q; require http (loopback only) or https", raw, u.Scheme)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("server.baseUrl %q uses cleartext http on a non-loopback host; the X-API-Key shared secret must not be sent over a cleartext wire", raw)
	}
	return nil
}

// validateBaseURL is New's contract-named alias for ValidateServerURL,
// kept so the panic message in New reads naturally. New panics on a
// non-nil result; callers wanting typed errors should call
// ValidateServerURL directly.
func validateBaseURL(raw string) error {
	return ValidateServerURL(raw)
}

// isLoopbackHost reports whether h is one of the loopback host names an
// operator is reasonably likely to type: 127.0.0.1, ::1 (any zone), and
// "localhost". Hostnames are compared case-insensitively. net.ParseIP
// handles bracketed-IPv6 ("::1") the same way url.URL.Hostname() strips
// the brackets, so no extra unwrapping is needed here.
//
// Duplicates internal/config.isLoopbackHost rather than importing it:
// internal/branchdam must stay import-clean from internal/config (the
// dependency direction is the other way around -- internal/config is a
// leaf that knows about neither the HTTP client nor the queue), so a
// small string-matching helper is the lightest correct option. The
// rule being duplicated is also exactly two lines and is unlikely to
// evolve independently.
func isLoopbackHost(h string) bool {
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// signRequest computes the S-3 replay-protection signature over the
// canonical string "method\npath\nnonce\ntimestamp\n" followed by the
// raw request body bytes, using HMAC-SHA256 keyed by the apiKey.
// For GET requests with query parameters (e.g. /api/v1/agent/check-content?fastHash=...),
// path includes the full raw query string as passed to the request URI
// (matching r.URL.RequestURI()). The server-side validator is a separate
// branchDAM PR gated on server.signedRequests; the client contract is what
// this package pins today (issue #95's "client-side nonce + signature,
// server-side validation as follow-up" split).
//
// Body bytes are appended after the trailing "\n" so a request with an
// empty body is a well-defined canonical string (no body bytes), not a
// malformed one -- a Hello() call signs over the empty byte slice.
//
// apiKey is the key the request is sent with, read once per request (see
// Client), so the signature and X-API-Key always agree.
func signRequest(apiKey, method, path, nonce, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(apiKey))
	mac.Write([]byte(method + "\n" + path + "\n" + nonce + "\n" + timestamp + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newReplayProtectionFields returns a fresh (timestamp, nonce) pair for
// a single outgoing request. Timestamp is UnixNano as a base-10 string
// (server clocks are expected to be NTP-synced; the server-side replay
// window will be a small multiple of typical NTP drift). Nonce is 16
// random bytes hex-encoded -- collision probability is negligible at
// 2^128 per pair, and uniqueness is asserted by a unit test.
//
// On crypto/rand failure the call returns an error rather than a
// deterministic nonce. The earlier implementation fell back to a
// timestamp-derived nonce so the request still went out, but a
// deterministic nonce defeats the entire S-3 protection: an attacker
// observing one signed request could forge a re-send with the same
// nonce/timestamp (and therefore the same signature), and the
// server-side replay window would also miss it because the signature
// matches. Dropping the request on rand.Read failure is the only way
// to keep the guarantee honest.
func newReplayProtectionFields() (timestamp, nonce string, err error) {
	ts := strconv.FormatInt(time.Now().UnixNano(), 10)
	var nonceBytes [16]byte
	if _, rerr := rand.Read(nonceBytes[:]); rerr != nil {
		return "", "", fmt.Errorf("branchdam: read crypto/rand for replay nonce: %w", rerr)
	}
	return ts, hex.EncodeToString(nonceBytes[:]), nil
}

// post issues a POST to c.baseURL+path with body JSON-marshalled (or no
// body at all if body is nil, matching /hello's empty struct{} input), and
// JSON-unmarshals a 2xx response into out (skipped if out is nil). A
// non-2xx response is returned as *HTTPError with Body set to the raw
// response text -- 401/503 bodies are plain text, not JSON, so this never
// attempts to parse them as anything but a string. HTTPError.Error()
// intentionally does NOT echo the body verbatim (S-7): the body is
// preserved as a field for Classification()'s fatal/transient matching
// but is not interpolated into error messages, so a server that reflects
// an X-API-Key or other secret back to the client cannot re-leak it via
// a slog/crash-dump path.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	_, err := c.do(ctx, http.MethodPost, path, body, out)
	return err
}

// get issues a GET to c.baseURL+path, JSON-unmarshalling a 2xx response into out
// (skipped if out is nil). path must be the exact request URI path + query string
// (e.g. constructed via url.Values.Encode) as transmitted to the server, matching
// the URI string signed in X-Signature. A non-2xx response is returned as *HTTPError.
func (c *Client) get(ctx context.Context, path string, out any) error {
	_, err := c.do(ctx, http.MethodGet, path, nil, out)
	return err
}

// do sends one replayable request (see post and get) and returns the API
// key the final attempt used. After each response observeAuth may ask for
// one retry with a different key: when a freshly rotated key is refused,
// or when the key changed while the request was in flight.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (string, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("branchdam: marshal request: %w", err)
		}
		bodyBytes = b
	}

	key := c.currentKey()
	for attempt := 0; ; attempt++ {
		status, respBody, err := c.send(ctx, method, path, key, body != nil, bodyBytes)
		if err != nil {
			return key, err
		}
		if retry := c.observeAuth(ctx, key, status); retry != "" && retry != key && attempt == 0 {
			key = retry
			continue
		}
		if status < 200 || status >= 300 {
			return key, &HTTPError{StatusCode: status, Body: string(bytes.TrimSpace(respBody))}
		}
		if out == nil || len(respBody) == 0 {
			return key, nil
		}
		if err := json.Unmarshal(respBody, out); err != nil {
			return key, fmt.Errorf("branchdam: decode response %s: %w", path, err)
		}
		return key, nil
	}
}

// send performs one HTTP round trip authenticated and signed with apiKey
// and returns the status code and the full response body.
func (c *Client) send(ctx context.Context, method, path, apiKey string, hasBody bool, bodyBytes []byte) (int, []byte, error) {
	var reqBody io.Reader
	if hasBody {
		reqBody = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("branchdam: build request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}

	timestamp, nonce, rerr := newReplayProtectionFields()
	if rerr != nil {
		return 0, nil, fmt.Errorf("branchdam: build replay protection fields: %w", rerr)
	}
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Nonce", nonce)
	req.Header.Set("X-Signature", signRequest(apiKey, req.Method, path, nonce, timestamp, bodyBytes))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("branchdam: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("branchdam: read response %s: %w", path, err)
	}
	return resp.StatusCode, respBody, nil
}

// ContentCheckQuery is the query of one GET /api/v1/agent/check-content
// call. The server picks the mode from which hash is set:
//
//   - FastHash (+ SizeBytes) without FullHash is the fast pre-screen: the
//     answer is only PossibleMatch, never which node matched.
//   - FullHash is the definitive lookup (FastHash and SizeBytes are then
//     format-checked but otherwise ignored).
//
// SizeBytes <= 0 is not sent ("unknown"). Servers predating the pre-screen
// reject a request without FullHash with 400.
type ContentCheckQuery struct {
	FastHash  string
	SizeBytes int64
	FullHash  string
}

// CheckContent calls GET /api/v1/agent/check-content with q.
// Returns (result, nil) on success; (zero, err) on network failure or a
// non-2xx status (*HTTPError). Callers treat any error as "not found"
// (fail-open) so ingest proceeds rather than blocking on the server.
func (c *Client) CheckContent(ctx context.Context, q ContentCheckQuery) (ContentCheckResult, error) {
	params := url.Values{}
	if q.FastHash != "" {
		params.Set("fastHash", q.FastHash)
	}
	if q.SizeBytes > 0 {
		params.Set("sizeBytes", strconv.FormatInt(q.SizeBytes, 10))
	}
	if q.FullHash != "" {
		params.Set("fullHash", q.FullHash)
	}
	path := "/api/v1/agent/check-content"
	if enc := params.Encode(); enc != "" {
		path += "?" + enc
	}
	var out ContentCheckResult
	if err := c.get(ctx, path, &out); err != nil {
		return ContentCheckResult{}, err
	}
	return out, nil
}
