package branchdam

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResolveSnapshot is a complete read of one Resolve database scope. The
// server reconciles it synchronously, so 200 means the graph transaction
// committed (unlike the asynchronous 202 event endpoint).
type ResolveSnapshot struct {
	AgentID                 string                      `json:"agentId"`
	ScopeID                 string                      `json:"scopeId"`
	Timelines               []ResolveSnapshotTimeline   `json:"timelines"`
	Memberships             []ResolveSnapshotMembership `json:"memberships"`
	LegacyTimelineNodeUUIDs []string                    `json:"legacyTimelineNodeUuids,omitempty"`
	RetireScopeID           string                      `json:"retireScopeId,omitempty"`
}

type ResolveSnapshotTimeline struct {
	TimelineID   string          `json:"timelineId"`
	NodeUUID     string          `json:"nodeUuid"`
	FilePath     string          `json:"filePath"`
	DisplayName  string          `json:"displayName"`
	EvidenceJSON json.RawMessage `json:"evidenceJson"`
}

type ResolveSnapshotMembership struct {
	TimelineID     string          `json:"timelineId"`
	MediaFilePath  string          `json:"mediaFilePath"`
	SourceNodeUUID string          `json:"sourceNodeUuid,omitempty"`
	EvidenceJSON   json.RawMessage `json:"evidenceJson,omitempty"`
}

type ResolveSnapshotResponse struct {
	Created           int `json:"created"`
	Refreshed         int `json:"refreshed"`
	Removed           int `json:"removed"`
	Unchanged         int `json:"unchanged"`
	Unresolved        int `json:"unresolved"`
	ReviewedConflicts int `json:"reviewedConflicts"`
}

func (c *Client) PostResolveSnapshot(ctx context.Context, snapshot ResolveSnapshot) (*ResolveSnapshotResponse, error) {
	if snapshot.AgentID == "" || snapshot.ScopeID == "" {
		return nil, fmt.Errorf("branchdam: Resolve snapshot requires agentId and scopeId")
	}
	var out ResolveSnapshotResponse
	if err := c.post(ctx, "/api/v1/agent/resolve-snapshot", snapshot, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
