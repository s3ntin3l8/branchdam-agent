package branchdam

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// mintEventUUID generates a UUIDv7 for transport-level event idempotency,
// falling back to UUIDv4 if v7 generation fails. Package-level variable so tests
// can verify the v4 fallback path without flaky clock manipulation.
var mintEventUUID = func() string {
	u, err := uuid.NewV7()
	if err != nil {
		return uuid.New().String()
	}
	return u.String()
}

// MintEventUUID generates a UUIDv7 for transport-level event idempotency,
// falling back to UUIDv4 if v7 generation fails.
func MintEventUUID() string {
	return mintEventUUID()
}

// postEvent marshals payload to JSON, double-encodes it into
// EventEnvelope.Payload as a string (sending it as a bare object is a 422 --
// AgentEventInput.Body.Payload is declared `json:"payload" required:"true"`
// as a Go string server-side), mints or preserves an EventUUID for transport-level
// idempotency, and POSTs the envelope. Returns the 202 response's eventId.
func (c *Client) postEvent(ctx context.Context, agentID, eventType string, payload any, eventUUID string) (*EventResponse, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("branchdam: marshal %s payload: %w", eventType, err)
	}

	if eventUUID == "" {
		eventUUID = mintEventUUID()
	}

	env := EventEnvelope{
		EventUUID: eventUUID,
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

// PostNodeCreated sends EVENT_NODE_CREATED with a client-minted EventUUID
// for transport-level idempotency on retries.
func (c *Client) PostNodeCreated(ctx context.Context, agentID string, payload NodeCreatedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeCreated, payload, "")
}

// PostNodeCreatedWithUUID sends EVENT_NODE_CREATED with an explicit EventUUID,
// guaranteeing that retries of the same logical event re-send the same UUID
// across network or drain retry boundaries.
func (c *Client) PostNodeCreatedWithUUID(ctx context.Context, agentID string, payload NodeCreatedPayload, eventUUID string) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeCreated, payload, eventUUID)
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
	return c.postEvent(ctx, agentID, EventEdgeAttached, payload, "")
}

// PostNodeMoved sends EVENT_NODE_MOVED.
func (c *Client) PostNodeMoved(ctx context.Context, agentID string, payload NodeMovedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeMoved, payload, "")
}

// PostNodeDeleted sends EVENT_NODE_DELETED.
func (c *Client) PostNodeDeleted(ctx context.Context, agentID string, payload NodeDeletedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventNodeDeleted, payload, "")
}

// PostPathRebased sends EVENT_PATH_REBASED. Prefer Client.Rebase
// (POST /api/v1/agent/rebase) when a synchronous result is needed -- this
// event-queue path is fire-and-forget like every other /events call.
func (c *Client) PostPathRebased(ctx context.Context, agentID string, payload PathRebasedPayload) (*EventResponse, error) {
	return c.postEvent(ctx, agentID, EventPathRebased, payload, "")
}
