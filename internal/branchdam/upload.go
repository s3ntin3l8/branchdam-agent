package branchdam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const uploadPath = "/api/v1/agent/upload"

// DefaultUploadIdleTimeout is how long an upload may go without the
// transport taking a single body byte before Upload gives up. It replaces
// a whole-exchange deadline: a multi-GB video legitimately takes far
// longer than any fixed total timeout, but a stream that stops moving
// for this long is dead (server stopped reading, link dropped without a
// reset, NAS behind the server stalled).
const DefaultUploadIdleTimeout = 2 * time.Minute

// DefaultUploadResponseTimeout bounds the wait for the response headers
// once the whole body has been sent. The server answers only after it
// has hashed, probed (exiftool/ffprobe/pHash, each up to 30s) and
// recorded the file, so this is deliberately generous.
const DefaultUploadResponseTimeout = 5 * time.Minute

// UploadSignMaxBytes is the largest upload body Upload signs. The S-3
// signature covers the whole body, so signing means holding the body in
// memory before the request starts; the server side buffers a signed body
// the same way and refuses one over agent.signedMaxBodyBytes (16 MiB by
// default) with 413. A bigger body could therefore never verify even if
// signed -- which is why the server exempts /api/v1/agent/upload from
// signature checks by default (agent.skipSignaturePaths). Bodies up to
// this size carry X-Timestamp/X-Nonce/X-Signature exactly like every
// other agent request; larger ones are sent with the API key only.
const UploadSignMaxBytes int64 = 16 * 1024 * 1024

// ErrUploadStalled is returned (wrapped) by Upload when the transport
// took no body bytes for the idle timeout.
var ErrUploadStalled = errors.New("branchdam: upload stalled")

// ErrUploadHashMismatch is returned (wrapped) by Upload when the server
// reports a BLAKE3 for the stored bytes that differs from
// UploadOptions.Blake3Hash. The server already refuses such an upload
// with 400 CHECKSUM_MISMATCH; this is the client-side belt to that brace.
var ErrUploadHashMismatch = errors.New("branchdam: server BLAKE3 does not match the uploaded bytes")

// WithUploadHTTPClient overrides the *http.Client Upload uses. It is
// separate from WithHTTPClient because the JSON calls want a short total
// timeout and the upload must not have one. Tests only -- see
// WithHTTPClient's SECURITY note, which applies equally here.
func WithUploadHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.uploadClient = hc }
}

// WithUploadIdleTimeout overrides DefaultUploadIdleTimeout (d <= 0 keeps
// the default).
func WithUploadIdleTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.uploadIdleTimeout = d
		}
	}
}

// uploadTransport is secureTransport plus the per-phase deadlines that
// stand in for the total timeout the upload client deliberately lacks:
// connect, TLS handshake and (after the body is sent) response headers.
// The body phase itself is guarded by Upload's idle watchdog. Like every
// other agent call it ignores HTTP(S)_PROXY, so uploads reach the server
// the same way the handshake does.
func uploadTransport() *http.Transport {
	tr := secureTransport()
	tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 30 * time.Second
	tr.ResponseHeaderTimeout = DefaultUploadResponseTimeout
	return tr
}

// newUploadHTTPClient is the default upload client: no Timeout (a total
// deadline would cap the size of file that can be uploaded at all).
func newUploadHTTPClient() *http.Client {
	return &http.Client{Transport: uploadTransport()}
}

// Upload streams raw media bytes to POST /api/v1/agent/upload and returns
// the server's *UploadResponse (node UUID, relativePath in the master
// archive, bytes written, BLAKE3 of what it stored).
//
// opts.Blake3Hash, when set, must be the BLAKE3-256 of body. The server
// checks the stored bytes against it (400 on mismatch) and, when a live
// node already has that hash, answers 200 DEDUPLICATED without storing
// anything -- so callers that know the hash up front should always send
// it. opts.SizeBytes, when > 0, is sent as Content-Length.
//
// There is no total deadline: the request runs until ctx is done, the
// body stops moving for the idle timeout (ErrUploadStalled), or the
// response headers do not arrive within DefaultUploadResponseTimeout of
// the body being sent.
func (c *Client) Upload(ctx context.Context, body io.Reader, opts UploadOptions) (*UploadResponse, error) {
	if opts.SourcePathHash != "" {
		h := strings.ToLower(strings.TrimSpace(opts.SourcePathHash))
		if len(h) != 64 || !isHex(h) {
			return nil, fmt.Errorf("branchdam: invalid SourcePathHash: must be 64 lowercase hex characters")
		}
		opts.SourcePathHash = h
	}
	if opts.Blake3Hash != "" {
		h := strings.ToLower(strings.TrimSpace(opts.Blake3Hash))
		if len(h) != 64 || !isHex(h) {
			return nil, fmt.Errorf("branchdam: invalid Blake3Hash: must be 64 lowercase hex characters")
		}
		opts.Blake3Hash = h
	}
	if body == nil {
		body = http.NoBody
	}

	upCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := c.uploadIdleTimeout
	if idle <= 0 {
		idle = DefaultUploadIdleTimeout
	}
	onIdle := func() {
		cancel(fmt.Errorf("%w: no bytes sent for %s", ErrUploadStalled, idle))
	}

	// Small bodies are read into memory so they can be signed; see
	// UploadSignMaxBytes. Reading one byte past the cap tells a small
	// body of unknown size apart from a large one without consuming more
	// than that of a large one. The read is bound by ctx and the idle
	// watchdog like the request itself.
	var signBody []byte
	if opts.SizeBytes <= UploadSignMaxBytes {
		prefix, err := readPrefix(upCtx, body, UploadSignMaxBytes+1, idle, onIdle)
		if err != nil {
			return nil, fmt.Errorf("branchdam: read upload body: %w", err)
		}
		if int64(len(prefix)) <= UploadSignMaxBytes {
			signBody = prefix
			body = bytes.NewReader(prefix)
		} else {
			body = io.MultiReader(bytes.NewReader(prefix), body)
		}
	}

	if signBody == nil {
		// Deliberate: the server's default skipSignaturePaths exempts
		// /upload. Logged so a 401 after a server narrows that exemption
		// can be tied to "only the large files".
		slog.Debug("branchdam: upload body above the signing cap; sent with the API key only",
			"filename", opts.Filename, "signMaxBytes", UploadSignMaxBytes)
	}
	watched := newIdleReader(body, idle, onIdle)
	defer watched.stop()

	req, err := http.NewRequestWithContext(upCtx, http.MethodPost, c.baseURL+uploadPath, watched)
	if err != nil {
		return nil, fmt.Errorf("branchdam: build upload request: %w", err)
	}
	// ContentLength 0 with a non-nil Body means "unknown" to the
	// transport (chunked encoding); an empty signed body is sent as
	// NoBody so it is a real zero-length request.
	switch {
	case signBody != nil && len(signBody) == 0:
		req.Body = http.NoBody
	case signBody != nil:
		req.ContentLength = int64(len(signBody))
	case opts.SizeBytes > 0:
		req.ContentLength = opts.SizeBytes
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
	if opts.SourcePathHash != "" {
		req.Header.Set("X-Source-Path-Hash", opts.SourcePathHash)
	}
	if signBody != nil {
		timestamp, nonce, rerr := newReplayProtectionFields()
		if rerr != nil {
			return nil, fmt.Errorf("branchdam: build replay protection fields: %w", rerr)
		}
		req.Header.Set("X-Timestamp", timestamp)
		req.Header.Set("X-Nonce", nonce)
		req.Header.Set("X-Signature", c.signRequest(http.MethodPost, uploadPath, nonce, timestamp, signBody))
	}

	hc := c.uploadClient
	if hc == nil {
		hc = newUploadHTTPClient()
	}
	resp, err := hc.Do(req)
	if err != nil {
		if cause := context.Cause(upCtx); cause != nil && errors.Is(cause, ErrUploadStalled) {
			return nil, fmt.Errorf("branchdam: upload: %w", cause)
		}
		return nil, fmt.Errorf("branchdam: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is done (or the server answered early, e.g. a pre-write
	// dedup): from here only the response read is outstanding. It gets
	// its own idle watchdog -- ResponseHeaderTimeout covers only the
	// header wait, and a server that sends headers and then stalls would
	// otherwise hang the ingest. Cancelling upCtx aborts the body read.
	watched.stop()
	respWatched := newIdleReader(io.LimitReader(resp.Body, maxUploadResponseBytes), idle, onIdle)
	defer respWatched.stop()

	respBody, err := io.ReadAll(respWatched)
	if err != nil {
		if cause := context.Cause(upCtx); cause != nil && errors.Is(cause, ErrUploadStalled) {
			return nil, fmt.Errorf("branchdam: read upload response: %w", cause)
		}
		return nil, fmt.Errorf("branchdam: read upload response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(respBody))}
	}

	var out UploadResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("branchdam: decode upload response: %w", err)
	}
	if opts.Blake3Hash != "" && out.Blake3Hash != "" && !strings.EqualFold(out.Blake3Hash, opts.Blake3Hash) {
		return nil, fmt.Errorf("%w: sent %s, server reports %s for node %s at %s",
			ErrUploadHashMismatch, opts.Blake3Hash, out.Blake3Hash, out.NodeUUID, out.RelativePath)
	}
	return &out, nil
}

// maxUploadResponseBytes bounds the upload response read: the JSON is a
// few hundred bytes, and an error page is not worth more than this.
const maxUploadResponseBytes = 1 << 20

// readPrefix reads up to n bytes of r, returning early with ctx's cause
// once ctx is done -- including when r stops producing bytes for idle,
// which calls onIdle. The read runs on its own goroutine because a Read
// blocked in the kernel (a hung network mount) ignores ctx; on an early
// return that goroutine is abandoned until the Read returns, and r must
// not be used again. Accepted cost: each upload cancelled that way holds
// one goroutine and the caller's file handle until the mount answers or
// the process exits -- nothing in user space can free a blocked read.
func readPrefix(ctx context.Context, r io.Reader, n int64, idle time.Duration, onIdle func()) ([]byte, error) {
	watched := newIdleReader(r, idle, onIdle)
	defer watched.stop()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(watched, n))
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		return res.b, res.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// idleReader wraps an upload body and calls onIdle once if no Read
// returns data for timeout. The http.Transport reads the body from its
// own goroutine as fast as the socket accepts it, so a Read that blocks
// (or simply is not called) for that long means the stream is stuck.
type idleReader struct {
	r       io.Reader
	timeout time.Duration

	mu       sync.Mutex
	timer    *time.Timer
	lastData time.Time // last Read that returned bytes
	stopped  bool
}

func newIdleReader(r io.Reader, timeout time.Duration, onIdle func()) *idleReader {
	ir := &idleReader{r: r, timeout: timeout, lastData: time.Now()}
	ir.timer = time.AfterFunc(timeout, func() {
		ir.mu.Lock()
		// A Read that returned data after this firing was scheduled
		// re-armed the timer; the stream is moving, so this firing is
		// stale and the re-armed one decides.
		idle := !ir.stopped && time.Since(ir.lastData) >= ir.timeout
		ir.mu.Unlock()
		if idle {
			onIdle()
		}
	})
	return ir
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	ir.mu.Lock()
	if !ir.stopped {
		if n > 0 {
			ir.lastData = time.Now()
			ir.timer.Reset(ir.timeout)
		}
		if err == io.EOF {
			// Fully handed to the transport: the response wait is
			// ResponseHeaderTimeout's job, not the idle watchdog's.
			ir.stopped = true
			ir.timer.Stop()
		}
	}
	ir.mu.Unlock()
	return n, err
}

func (ir *idleReader) stop() {
	ir.mu.Lock()
	ir.stopped = true
	ir.timer.Stop()
	ir.mu.Unlock()
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
