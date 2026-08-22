package branchdam

import (
	"context"
	"encoding/json"
	"fmt"
)

// postEvent marshals payload to JSON, double-encodes it into
// EventEnvelope.Payload as a string (sending it as a bare object is a 422 --
// AgentEventInput.Body.Payload is declared `json:"payload" required:"true"`
// as a Go string server-side), and POSTs the envelope. Returns the 202
// response's eventId. 202 means enqueued, never applied -- see EventResponse's
// doc comment and the plan's contract gap 3 (no failure feedback channel).
func (c *Client) postEvent(ctx context.Context, agentID, eventType string, payload any) (*EventResponse, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("branchdam: marshal %s payload: %w", eventType, err)
	}

	env := EventEnvelope{
		AgentID:   agentID,
		EventType: eventType,
		Payload:   string(payloadJSON),
	}

	var out EventResponse
	if err := c.post(ctx, "/api/v1/agent/events", env, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PostNodeCreated sends EVENT_NODE_CREATED. Event submission is not
// idempotent at the transport level (plan gap 1: the server mints its own
// event_uuid, ignoring any client-side one), so callers must mint a stable
// NodeUUID before the first attempt and rely on entity-level idempotency --
// a re-sent EVENT_NODE_CREATED for an existing nodeUuid is a silent
// no-op server-side, and a retry with corrected fields is silently ignored
// (first write wins).
func (c *Client) PostNodeCreated(ctx context.Context, agentID string, payload NodeCreatedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeCreated, payload)
}

// PostEdgeAttached sends EVENT_EDGE_ATTACHED, after running
// ValidateEdgeAttached against payload -- the server never validates this
// payload at enqueue time (202 regardless of content), so this is the only
// place a bad confidence/tier/relationshipType/reviewState is caught before
// it burns retries and lands FAILED with no feedback channel. A re-sent edge
// for the same (source, target, relationship) is idempotent but never
// refreshes confidence or evidence -- get it right the first time.
func (c *Client) PostEdgeAttached(ctx context.Context, agentID string, payload EdgeAttachedPayload) (*EventResponse, error) {
	if err := ValidateEdgeAttached(payload); err != nil {
		return nil, err
	}
	return c.postEvent(ctx, agentID, EventEdgeAttached, payload)
}

// PostNodeMoved sends EVENT_NODE_MOVED.
func (c *Client) PostNodeMoved(ctx context.Context, agentID string, payload NodeMovedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeMoved, payload)
}

// PostNodeDeleted sends EVENT_NODE_DELETED.
func (c *Client) PostNodeDeleted(ctx context.Context, agentID string, payload NodeDeletedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeDeleted, payload)
}

// PostPathRebased sends EVENT_PATH_REBASED. Prefer Client.Rebase
// (POST /api/v1/agent/rebase) when a synchronous result is needed -- this
// event-queue path is fire-and-forget like every other /events call.
func (c *Client) PostPathRebased(ctx context.Context, agentID string, payload PathRebasedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventPathRebased, payload)
}
