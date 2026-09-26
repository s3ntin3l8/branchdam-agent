package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

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
		say("Pairing failed: " + agentlog.Sanitize(err.Error()))
	}
}
