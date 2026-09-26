package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/sessiontoken"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// deepLinkForwardTimeout bounds the hand-off to a running tray. The tray
// answers as soon as the URL validates (its confirmation dialog runs
// detached), so this only has to cover a loopback round trip.
const deepLinkForwardTimeout = 10 * time.Second

// Seams so tests never reach a real tray, dialog backend, or server.
var (
	deepLinkForward    = forwardDeepLinkToTray
	deepLinkPairConfig = pairConfig
)

// trayForwardClient never follows redirects: the request body carries the API
// key, and a 307 from whatever answers on the port would forward it elsewhere.
var trayForwardClient = &http.Client{
	Timeout:       deepLinkForwardTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// errNoTray means no tray answered on the loopback API; the caller falls back
// to confirming and pairing in this process.
var errNoTray = errors.New("no running tray")

// runDeepLinkPair handles `pair -deeplink <url>`: a branchdam:// URL handed to
// this process by the OS protocol handler (Windows/Linux argv path; macOS uses
// an Apple Event, see internal/tray). Any web page can trigger such a launch,
// so it is NEVER paired silently:
//
//  1. A running tray owns the confirmation dialog and the config reload, so the
//     URL is handed to it (POST /api/actions/pair, source "deeplink").
//  2. With no tray running, this process shows the confirmation itself, then
//     pairs. With no dialog available it refuses (fail closed).
//
// Output goes to dialogs, not stderr: the Windows GUI binary has no console.
func runDeepLinkPair(configFlag, rawURL string, timeout time.Duration) int {
	run, _, _ := trayDialogSetup()
	fail := func(msg string) int {
		slog.Warn("deep-link pairing failed", "err", agentlog.Sanitize(msg))
		_, _, _ = run(context.Background(), "-kind", "error", "-title", "branchDAM Agent", "-message", "Pairing failed: "+msg)
		return 1
	}

	parsed, err := branchdam.ParsePairingURL(rawURL)
	if err != nil {
		return fail(err.Error())
	}
	path, err := config.ResolvePath(configFlag)
	if err != nil {
		return fail("resolve config path: " + err.Error())
	}
	cfg, err := config.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil // fresh machine: Load returned defaults, Patch creates the file.
	}
	if err != nil {
		return fail("load config: " + err.Error())
	}

	switch err := deepLinkForward(cfg.Tray.StatusAddrOrDefault(), rawURL); {
	case err == nil:
		return 0 // the tray confirms, pairs, and notifies.
	case !errors.Is(err, errNoTray):
		return fail(err.Error())
	}

	body := fmt.Sprintf("Server: %s\nAgent ID: %s", parsed.Server, parsed.Agent)
	if cur := cfg.Server.BaseURL; cur != "" {
		// Same guard as tray.confirmAndPair: a stored baseUrl with bidi
		// characters or a YAML newline must not forge the dialog.
		if !branchdam.IsDisplayable(cur) {
			cur = "(unreadable)"
		}
		body += fmt.Sprintf("\n\nThis replaces the current server (%s).", cur)
	}
	body += "\n\nOnly continue if you just started this from your branchDAM server."
	// Direct confirm, not gated by tray.confirmDestructive: see tray.confirmAndPair.
	if !trayConfirm(run)(context.Background(), "Pair with branchDAM server?", body) {
		slog.Info("deep-link pairing declined or no dialog available")
		return 1
	}
	if err := deepLinkPairConfig(path, parsed.Server, parsed.Key, parsed.Agent, timeout, cfg); err != nil {
		return fail(err.Error())
	}
	_, _, _ = run(context.Background(), "-kind", "notify", "-title", "branchDAM Agent",
		"-message", "Paired with "+parsed.Server+". Start the branchDAM tray to begin.")
	return 0
}

// forwardDeepLinkToTray POSTs rawURL to the tray's loopback API. errNoTray
// means nothing answered (no token file, or connection refused); any other
// error is the tray's own rejection and must not fall back to a local write.
func forwardDeepLinkToTray(addr, rawURL string) error {
	// The body carries the API key: never dial anything but this machine. A
	// widened tray.statusAddr can't accept the hand-off anyway (the route is
	// loopback-only), so fall back to the local confirmation.
	if !isLoopbackAddr(addr) {
		slog.Warn("tray status address is not loopback; not forwarding the pairing URL", "addr", agentlog.Sanitize(addr))
		return errNoTray
	}
	token, err := sessiontoken.Read()
	if err != nil {
		return errNoTray
	}
	payload, err := json.Marshal(struct {
		URL    string `json:"url"`
		Source string `json:"source"`
	}{URL: rawURL, Source: tray.PairSourceDeepLink})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deepLinkForwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/actions/pair", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := trayForwardClient.Do(req)
	if err != nil {
		// Only a failed dial proves the request never reached a tray (stale
		// token file from one that has exited). Anything later -- e.g. a
		// timeout after the tray accepted the POST -- must NOT fall back to a
		// second local confirmation.
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return errNoTray
		}
		return fmt.Errorf("could not confirm the running tray received the request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusOK:
		// Only a tray that predates deep-link confirmation answers 200: it
		// ignored source and already wrote the pairing without asking.
		return errors.New("the running tray is an older version that paired without asking for confirmation; update the agent")
	}
	// Deliberately not echoing the response body: a non-tray listener on the
	// port could reflect the request (API key) onto the screen.
	return fmt.Errorf("the running tray rejected the request (HTTP %d)", resp.StatusCode)
}

// isLoopbackAddr reports whether host:port names the local machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
