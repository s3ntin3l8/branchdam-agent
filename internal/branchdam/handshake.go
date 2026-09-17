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
// this agent has no device-pairing flow yet (see HandshakeRequest.CurrentKeyID's
// doc comment), so there is no key_id for it to report. The server treats an
// absent CurrentKeyID the same as an older, pre-pairing client: it never
// sends back a PendingRotation hint. If resp.PendingRotation does arrive
// (e.g. a future caller starts populating CurrentKeyID), it is logged at
// Warn -- loud enough for an operator/on-call to notice -- since acting on
// it (persisting the new key) is a separate, not-yet-implemented mechanism;
// see issue #235.
func (c *Client) Handshake(ctx context.Context, req HandshakeRequest) (*HandshakeResponse, error) {
	var out HandshakeResponse
	if err := c.post(ctx, "/api/v1/agent/handshake", req, &out); err != nil {
		return nil, err
	}
	if out.PendingRotation != nil {
		slog.Warn("branchdam: server sent a key-rotation hint; automatic rotation is not implemented, manual re-pairing is required",
			"newKeyId", out.PendingRotation.KeyID,
			"previousKeyExpiresAtUnix", out.PendingRotation.PreviousKeyExpiresAt,
			"apiKeyProvided", out.PendingRotation.APIKey != "",
		)
	}
	return &out, nil
}
