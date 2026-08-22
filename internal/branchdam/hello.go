package branchdam

import "context"

// Hello calls POST /api/v1/agent/hello -- the lightest possible reachability
// + auth + version check. Registered huma.Post-only server-side; a bare GET
// returns 405 (doc bug #1 in the plan's housekeeping list). Takes no request
// body.
func (c *Client) Hello(ctx context.Context) (*HelloResponse, error) {
	var out HelloResponse
	if err := c.post(ctx, "/api/v1/agent/hello", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
