package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// Deep-link pairing sentinels. Both mean "nothing was written".
var (
	// ErrPairDeclined: the operator answered the confirmation dialog with
	// anything but a clean OK.
	ErrPairDeclined = errors.New("pairing declined")
	// ErrPairNoConfirm: no confirmation dialog is wired, so a deep-link
	// pair is refused (fail closed) rather than proceeding silently.
	ErrPairNoConfirm = errors.New("no confirmation dialog available")
)

// PairSourceDeepLink is the pairActionRequest.Source value a caller sets when
// the URL came from an OS protocol handoff (a web page can fire one) rather
// than the operator pasting it into the Pair dialog themselves.
const PairSourceDeepLink = "deeplink"

// confirmAndPair is the single confirm-then-Pair path shared by the macOS
// GetURL Apple Event handler (handleDeepLink) and POST /api/actions/pair when
// the request is marked as a deep link.
//
// The confirmation is ALWAYS shown and calls confirm directly -- never
// confirmDestructiveAction, whose ConfirmDestructive() setting the operator
// can switch off. Any web page can trigger a branchdam:// link; without this
// prompt one could silently re-point the agent at an attacker's server. A nil
// confirm refuses (fail closed).
func confirmAndPair(
	ctx context.Context,
	settings Settings,
	confirm func(ctx context.Context, title, body string) bool,
	rawURL string,
) (branchdam.PairingURL, error) {
	parsed, err := branchdam.ParsePairingURL(rawURL)
	if err != nil {
		return branchdam.PairingURL{}, err
	}
	if confirm == nil {
		return parsed, ErrPairNoConfirm
	}
	body := fmt.Sprintf("Server: %s\nAgent ID: %s", parsed.Server, parsed.Agent)
	if cur := settings.Snapshot().ServerBaseURL; cur != "" {
		if !branchdam.IsDisplayable(cur) {
			cur = "(unreadable)"
		}
		body += fmt.Sprintf("\n\nThis replaces the current server (%s).", cur)
	}
	body += "\n\nOnly continue if you just started this from your branchDAM server."
	if !confirm(ctx, "Pair with branchDAM server?", body) {
		return parsed, ErrPairDeclined
	}
	return parsed, settings.Pair(parsed.Server, parsed.Key, parsed.Agent)
}

// pairMu serializes deep-link pairs: the Apple Event consumer and the loopback
// endpoint can both start one, and two confirmation dialogs must not overlap.
var pairMu sync.Mutex

// deepLinkQueue coalesces deep-link requests to the latest one. The Apple
// Event consumer and every POST /api/actions/pair (source "deeplink") funnel
// through submitDeepLink, so while a confirmation is open, further links
// replace one another in a single pending slot instead of stacking a dialog
// each -- a page firing many links cannot queue dialogs ahead of the
// operator's own click.
var deepLinkQueue struct {
	mu      sync.Mutex
	pending string
	has     bool
	running bool
}

// submitDeepLink runs handleDeepLink for rawURL, or, if another deep link is
// already being handled, records rawURL as the single pending one (replacing
// any earlier pending link) and returns; the running worker picks it up next.
func submitDeepLink(
	ctx context.Context,
	rawURL string,
	settings Settings,
	confirm func(ctx context.Context, title, body string) bool,
	notify func(ctx context.Context, title, message string),
) {
	q := &deepLinkQueue
	q.mu.Lock()
	q.pending, q.has = rawURL, true
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()
	for {
		q.mu.Lock()
		if !q.has {
			q.running = false
			q.mu.Unlock()
			return
		}
		u := q.pending
		q.pending, q.has = "", false
		q.mu.Unlock()
		handleDeepLink(ctx, u, settings, confirm, notify)
	}
}

// pairFailureSummary is the SHORT toast text for a failed deep-link pair. It
// leads with the cause and the fix, because macOS truncates long banners from
// the end (the full Post "url": dial tcp ... chain is ~380 characters and would
// push any hint out of view); the complete error is in the agent log.
func pairFailureSummary(goos string, err error) string {
	const pointer = " See the agent log for details."
	var httpErr *branchdam.HTTPError
	switch {
	case errors.As(err, &httpErr) && (httpErr.StatusCode == 401 || httpErr.StatusCode == 403):
		return "Pairing failed: the server rejected the key." + pointer
	case errors.As(err, &httpErr):
		return fmt.Sprintf("Pairing failed: the server replied HTTP %d.", httpErr.StatusCode) + pointer
	case errors.Is(err, branchdam.ErrPairingHelloFailed):
		if goos == "darwin" && errors.Is(err, syscall.EHOSTUNREACH) {
			return "Pairing failed: could not reach the server. Allow branchDAM in System Settings > Privacy & Security > Local Network, then try again."
		}
		return "Pairing failed: could not reach the server." + pointer
	}
	msg := agentlog.Sanitize(err.Error())
	if r := []rune(msg); len(r) > 120 {
		msg = string(r[:120]) + "..."
	}
	return "Pairing failed: " + msg + pointer
}

// handleDeepLink pairs from a branchdam:// URL delivered by the OS and reports
// the outcome through notify. The raw URL embeds the API key, so it is never
// logged or shown; errors from ParsePairingURL already redact it.
func handleDeepLink(
	ctx context.Context,
	rawURL string,
	settings Settings,
	confirm func(ctx context.Context, title, body string) bool,
	notify func(ctx context.Context, title, message string),
) {
	pairMu.Lock()
	defer pairMu.Unlock()
	say := func(msg string) {
		if notify != nil {
			notify(ctx, "branchDAM Agent", msg)
		}
	}
	parsed, err := confirmAndPair(ctx, settings, confirm, rawURL)
	switch {
	case err == nil:
		slog.Info("paired via deep link", "server", agentlog.Sanitize(parsed.Server), "agent", agentlog.Sanitize(parsed.Agent))
		say("Paired with " + agentlog.Sanitize(parsed.Server))
	case errors.Is(err, ErrPairDeclined):
		slog.Info("deep-link pairing declined")
	default:
		slog.Warn("deep-link pairing failed", "err", agentlog.Sanitize(err.Error()))
		say(pairFailureSummary(runtime.GOOS, err))
	}
}
