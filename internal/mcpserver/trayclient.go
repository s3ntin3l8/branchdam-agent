package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
)

const (
	// trayHTTPTimeout sits past the tray's two-minute drain click budget so
	// a finished pass can still return its JSON.
	trayHTTPTimeout = 2*time.Minute + 15*time.Second

	trayNotRunning   = "tray is not running"
	trayUnsupported  = "tray is not supported on this platform"
	maxTrayBodyBytes = 1 << 20
)

const trayAvailabilityNote = " If the tray is not running, the result is the text \"tray is not running\", not an error. On Linux the result is the text \"tray is not supported on this platform\"."

// trayClient talks to the tray loopback API. It reads the session token on
// every call and never logs it. It does not import internal/tray.
type trayClient struct {
	addr        string
	resolveAddr func() (string, error)
	baseURL     string
	readToken   func() (string, error)
	http        *http.Client
	goos        string
}

func (o Options) tray() *trayClient {
	goos := o.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	read := o.ReadToken
	if read == nil {
		read = sessiontoken.Read
	}
	addr := o.StatusAddr
	if addr == "" {
		addr = config.DefaultStatusAddr
	}
	return &trayClient{
		addr:        addr,
		resolveAddr: o.ResolveStatusAddr,
		baseURL:     o.trayBaseURL,
		readToken:   read,
		http:        o.HTTPClient,
		goos:        goos,
	}
}

func (c *trayClient) call(ctx context.Context, method, path string, body any) (*sdkmcp.CallToolResult, map[string]any, error) {
	if c.goos == "linux" {
		return textResult(trayUnsupported), nil, nil
	}
	addr := c.addr
	if c.resolveAddr != nil {
		resolved, err := c.resolveAddr()
		if err != nil {
			return nil, nil, err
		}
		addr = resolved
	}
	addr = normalizeStatusAddr(addr)
	if err := requireLoopback(addr); err != nil {
		return nil, nil, err
	}
	token, err := c.readToken()
	if err != nil {
		if os.IsNotExist(err) {
			return textResult(trayNotRunning), nil, nil
		}
		return nil, nil, fmt.Errorf("read session token: %w", err)
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
	}
	base := c.baseURL
	if base == "" {
		base = "http://" + addr
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, reader)
	if err != nil {
		return nil, nil, err
	}
	// Pin Host to the configured loopback address. The dial target may be a
	// test server; the header the tray checks must still name its own port.
	req.Host = addr
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if isConnRefused(err) {
			return textResult(trayNotRunning), nil, nil
		}
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTrayBodyBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read tray response: %w", err)
	}
	if len(raw) > maxTrayBodyBytes {
		return nil, nil, errors.New("tray response is too large")
	}
	msg := strings.TrimSpace(string(raw))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg == "" {
			msg = resp.Status
		}
		return nil, nil, fmt.Errorf("tray returned %s: %s", resp.Status, msg)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, nil, errors.New("tray returned a response that was not a JSON object")
	}
	text := msg
	if text == "" {
		text = resp.Status
	}
	return textResult(text), out, nil
}

func (c *trayClient) httpClient() *http.Client {
	if c.http == nil {
		return &http.Client{Timeout: trayHTTPTimeout, CheckRedirect: refuseRedirect}
	}
	clone := *c.http
	clone.CheckRedirect = refuseRedirect
	return &clone
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func requireLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || host == "" {
		return fmt.Errorf("tray status address %q is not a loopback host:port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("tray status address %q is not a loopback host:port", addr)
}

func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// Windows reports WSAECONNREFUSED as "actively refused", and that
	// errno does not unwrap to syscall.ECONNREFUSED.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused")
}

// normalizeStatusAddr matches the tray's rewrite of a bare ":port" (bind
// all interfaces) to 127.0.0.1:port, and an empty address to the default.
// Duplicated here so this package does not import internal/tray.
func normalizeStatusAddr(addr string) string {
	if addr == "" {
		return config.DefaultStatusAddr
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}
