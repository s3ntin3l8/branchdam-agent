package branchdam

import "context"

// Handshake calls POST /api/v1/agent/handshake. Per the plan's contract gap
// 2, this is NOT a resume mechanism despite its name: req.ClientVersion and
// req.LastProcessedEventUUID are accepted and never read server-side,
// resp.PendingEventsCount is server-global (not scoped to req.AgentID), and
// resp.AcknowledgedEventUUID only ever names a PROCESSED row -- it silently
// skips any FAILED one. Useful for a log line and a server-version check,
// nothing more.
func (c *Client) Handshake(ctx context.Context, req HandshakeRequest) (*HandshakeResponse, error) {
	var out HandshakeResponse
	if err := c.post(ctx, "/api/v1/agent/handshake", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
