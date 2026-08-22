package branchdam

import "context"

// NodeStatus calls POST /api/v1/agent/node-status -- the one read-only
// agent-facing route. Never resolves a filesystem path, never touches the
// server's storage.Guard; it is a pure lookup of what the server currently
// knows about each requested NodeUUID. Used by the prune subcommand to
// decide whether it's safe to delete a local-edit mirror of a file already
// durably archived: only once the server reports the node live
// (ACTIVE/HIDDEN) and hash-verified. req.NodeUUIDs must not exceed 200 --
// the server refuses a larger batch with a 400.
func (c *Client) NodeStatus(ctx context.Context, req NodeStatusRequest) (*NodeStatusResponse, error) {
	var out NodeStatusResponse
	if err := c.post(ctx, "/api/v1/agent/node-status", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
