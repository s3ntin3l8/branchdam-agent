package branchdam

import (
	"context"
	"fmt"
)

// SendTelemetry dispatches workstation scratch storage telemetry and prune stats
// to POST /api/v1/agent/telemetry.
func (c *Client) SendTelemetry(ctx context.Context, in AgentTelemetryInput) (*AgentTelemetryOutput, error) {
	var out AgentTelemetryOutput
	if err := c.post(ctx, "/api/v1/agent/telemetry", in, &out); err != nil {
		return nil, fmt.Errorf("branchdam: send telemetry: %w", err)
	}
	return &out, nil
}
