package branchdam

import "context"

// Rebase calls POST /api/v1/agent/rebase -- synchronous, unlike the
// /events queue. Returns Status "REBASED" (NodeUUID already known -- path,
// location, and mtime updated in place, content hashes and lineage edges
// preserved) or "CREATED" (NodeUUID unknown -- the agent is treated as the
// source of truth for an offline-staged file). A target resolving to a
// read-only, non-TIER3_MASTER_ARCHIVE location, or a TIER3_MASTER_ARCHIVE
// location where the file does not exist yet, is refused with a 400
// (surfaced as *HTTPError); an ARCHIVED NodeUUID is refused the same way, to
// avoid resurrecting a superseded version.
func (c *Client) Rebase(ctx context.Context, req RebaseRequest) (*RebaseResponse, error) {
	var out RebaseResponse
	if err := c.post(ctx, "/api/v1/agent/rebase", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
