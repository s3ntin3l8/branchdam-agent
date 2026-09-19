package branchdam

import (
	"context"
	"log/slog"
)

// Handshake calls POST /api/v1/agent/handshake. Per the plan's contract gap
// 2, this is NOT a resume mechanism despite its name: req.ClientVersion and
// req.LastProcessedEventUUID are accepted and never read server-side,
// resp.PendingEventsCount is server-global (not scoped to req.AgentID), and
// resp.AcknowledgedEventUUID only ever names a PROCESSED row -- it silently
// skips any FAILED one. Useful for a log line and a server-version check,
// nothing more.
//
// req.CurrentKeyID is left unset by every caller in this codebase today --
// this agent persists its paired agent_id via `branchdam-agent pair` (issue
// #453 PR B) but does not yet track its own device_pairing_keys row id, so
// there is no key_id for it to report. The server treats an absent
// CurrentKeyID the same as an older, pre-pairing client: it never sends
// back a PendingRotation hint. If resp.PendingRotation does arrive (e.g. a
// future caller starts populating CurrentKeyID), it is logged at Warn --
// loud enough for an operator/on-call to notice -- and the message points
// at the `pair` subcommand, since acting on it (persisting the new key)
// requires re-running `branchdam-agent pair` with a fresh branchdam:// URL
// from the SPA's Companion Pairing modal; the server's PendingRotation
// response deliberately omits the plaintext (see
// branchdam-server's pendingRotationDTO), so the agent cannot auto-rotate.
// See issue #235 for the future persistence-of-key-id iteration.
func (c *Client) Handshake(ctx context.Context, req HandshakeRequest) (*HandshakeResponse, error) {
	var out HandshakeResponse
	if err := c.post(ctx, "/api/v1/agent/handshake", req, &out); err != nil {
		return nil, err
	}
	if out.PendingRotation != nil {
		slog.Warn("branchdam: server sent a key-rotation hint; re-run `branchdam-agent pair <new-branchdam-url>` from the Companion Pairing modal to pick up the rotated key. Auto-rotation is not implemented (issue #235); the server's PendingRotation response does not include the plaintext.",
			"newKeyId", out.PendingRotation.KeyID,
			"previousKeyExpiresAtUnix", out.PendingRotation.PreviousKeyExpiresAtUnix,
			"apiKeyProvided", out.PendingRotation.APIKey != "",
		)
	}
	return &out, nil
}
