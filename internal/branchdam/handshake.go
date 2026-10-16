package branchdam

import (
	"context"
	"net/http"
)

// handshakePath is POST /api/v1/agent/handshake.
const handshakePath = "/api/v1/agent/handshake"

// Handshake calls POST /api/v1/agent/handshake. Per the plan's contract gap
// 2, this is NOT a resume mechanism despite its name: req.ClientVersion and
// req.LastProcessedEventUUID are accepted and never read server-side,
// resp.PendingEventsCount is server-global (not scoped to req.AgentID), and
// resp.AcknowledgedEventUUID only ever names a PROCESSED row -- it silently
// skips any FAILED one.
//
// It is also where rotated API keys arrive (branchDAM S13). When the
// response carries pendingRotation, Handshake opens the sealed key with the
// key this request used, stores it through the client's KeyPersister and
// switches to it, then adopts it right away with a second handshake and
// returns that response instead. Any step that fails leaves the client on
// its current key and returns the first response; see keyrotation.go for
// the whole sequence and the fallback when the server refuses the new key.
// A client built without WithKeyPersister only logs that a rotation is
// pending.
func (c *Client) Handshake(ctx context.Context, req HandshakeRequest) (*HandshakeResponse, error) {
	var out HandshakeResponse
	usedKey, err := c.do(ctx, http.MethodPost, handshakePath, req, &out)
	if err != nil {
		return nil, err
	}
	if out.PendingRotation != nil {
		if confirmed := c.adoptRotation(ctx, req, usedKey, out.PendingRotation); confirmed != nil {
			return confirmed, nil
		}
	}
	return &out, nil
}
