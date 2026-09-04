// Package branchdam is the REST client for branchDAM's agent-server
// contract (POST /api/v1/agent/{hello,handshake,events,rebase}). Every
// struct in this file is a byte-for-byte mirror of the server's DTOs --
// internal/agent/types.go (payload structs) and internal/httpapi/routes.go
// (request/response envelopes) in the branchdam repo, commit c570690's
// tree as read on 2026-08-22 (contract version pinned below). Nothing under
// branchdam's internal/ is importable cross-module, so this is a
// hand-written, hand-synced mirror -- the same accepted boundary as
// branchdam's own web/src/api/types.ts vs. its Go backend. Get a field name
// wrong here and it either 422s (Huma schema validation) or silently
// vanishes (an extra omitempty on a field the server expects); there is no
// compiler to catch either.
package branchdam

import "encoding/json"

// ContractVersion pins the specific branchdam commit this client's DTOs were
// built against, per issue #62's AC ("pin the specific contract version").
// Bump this deliberately -- alongside the structs below -- whenever
// branchdam's internal/agent/types.go or internal/httpapi/routes.go's agent
// DTOs change; conformance_test.go asserts it matches the committed golden
// fixture.
const ContractVersion = "branchdam@c570690"

// Event type constants, matching the event_queue.event_type CHECK
// constraint (internal/agent/types.go in branchdam).
const (
	EventNodeCreated  = "EVENT_NODE_CREATED"
	EventEdgeAttached = "EVENT_EDGE_ATTACHED"
	EventNodeMoved    = "EVENT_NODE_MOVED"
	EventNodeDeleted  = "EVENT_NODE_DELETED"
	EventPathRebased  = "EVENT_PATH_REBASED"
)

// Relationship types accepted by EdgeAttachedPayload.RelationshipType. Not
// validated by the Go server -- an unrecognized value hits a SQLite CHECK
// and is fatal with zero retries, so ValidateEdgeAttached below checks this
// client-side before ever sending the request.
const (
	RelationshipDerivedFrom    = "DERIVED_FROM"
	RelationshipFinalExport    = "FINAL_EXPORT"
	RelationshipProxyOf        = "PROXY_OF"
	RelationshipProjectSidecar = "PROJECT_SIDECAR"
	RelationshipDuplicateOf    = "DUPLICATE_OF"
)

// NodeCreatedPayload is the payload for EVENT_NODE_CREATED, marshalled to a
// JSON string and set as EventEnvelope.Payload (double-encoded -- sending it
// as a bare JSON object in the envelope is a 422). Mirrors
// internal/agent/types.go's NodeCreatedPayload field-for-field, including
// the two non-pointer, non-omitempty fields (NodeUUID, FilePath -- always on
// the wire), the four non-pointer omitempty fields (FileName, FileExt,
// SizeBytes, MtimeUnix -- vanish from the JSON at their zero value), and the
// rest as *T omitempty (nil means "not sent," not "sent as zero value").
type NodeCreatedPayload struct {
	NodeUUID string `json:"nodeUuid"`
	// StorageLocationID is deprecated and ignored server-side: the location
	// is always re-derived from FilePath via storage.Guard.Resolve. Kept
	// here only so an existing payload still round-trips; never send it
	// expecting effect.
	StorageLocationID  int64   `json:"storageLocationId,omitempty"`
	FilePath           string  `json:"filePath"`
	FileName           string  `json:"fileName,omitempty"`
	FileExt            string  `json:"fileExt,omitempty"`
	SizeBytes          int64   `json:"sizeBytes,omitempty"`
	MtimeUnix          int64   `json:"mtimeUnix,omitempty"`
	FastHash           *string `json:"fastHash,omitempty"`
	FullHash           *string `json:"fullHash,omitempty"`
	Phash              *int64  `json:"phash,omitempty"`
	CameraModel        *string `json:"cameraModel,omitempty"`
	CameraSerial       *string `json:"cameraSerial,omitempty"`
	LensModel          *string `json:"lensModel,omitempty"`
	CapturedAtUnix     *int64  `json:"capturedAtUnix,omitempty"`
	OriginalDocumentID *string `json:"originalDocumentId,omitempty"`
	DocumentID         *string `json:"documentId,omitempty"`
	DerivedFromID      *string `json:"derivedFromId,omitempty"`
	FilenameStem       *string `json:"filenameStem,omitempty"`
	// GPSLatitude/GPSLongitude are signed decimal degrees (Composite:
	// GPSLatitude/Longitude), not the unsigned EXIF: magnitude.
	GPSLatitude  *float64 `json:"gpsLatitude,omitempty"`
	GPSLongitude *float64 `json:"gpsLongitude,omitempty"`
}

// EdgeAttachedPayload is the payload for EVENT_EDGE_ATTACHED. Mirrors
// internal/agent/types.go's EdgeAttachedPayload exactly. EvidenceJSON is a
// JSON *object* (json.RawMessage) -- the opposite encoding from the
// envelope's own Payload field, which is a JSON *string*. See
// ValidateEdgeAttached for the hard gates the server enforces (some fatally,
// with zero retries) that are not caught by Huma schema validation at
// enqueue time.
type EdgeAttachedPayload struct {
	SourceNodeUUID   string          `json:"sourceNodeUuid"`
	TargetNodeUUID   string          `json:"targetNodeUuid"`
	SourceNodeID     int64           `json:"sourceNodeId,omitempty"`
	TargetNodeID     int64           `json:"targetNodeId,omitempty"`
	RelationshipType string          `json:"relationshipType"`
	Confidence       float64         `json:"confidence,omitempty"`
	Tier             int64           `json:"tier,omitempty"`
	Resolver         string          `json:"resolver,omitempty"`
	EvidenceJSON     json.RawMessage `json:"evidenceJson,omitempty"`

	// ReviewState is deprecated and ignored: review_state is always derived
	// server-side from (Confidence, Tier). Setting CONFIRMED/REJECTED here
	// is a fatal, zero-retry error -- never set it (see ValidateEdgeAttached).
	// Kept only so an existing payload still parses.
	ReviewState string `json:"reviewState,omitempty"`
}

// NodeMovedPayload is the payload for EVENT_NODE_MOVED.
type NodeMovedPayload struct {
	NodeUUID    string `json:"nodeUuid"`
	NewFilePath string `json:"newFilePath"`
	NewFileName string `json:"newFileName,omitempty"`
	// NewStorageLocationID is deprecated and ignored -- see
	// NodeCreatedPayload.StorageLocationID.
	NewStorageLocationID int64 `json:"newStorageLocationId,omitempty"`
	MtimeUnix            int64 `json:"mtimeUnix,omitempty"`
}

// NodeDeletedPayload is the payload for EVENT_NODE_DELETED.
type NodeDeletedPayload struct {
	NodeUUID string `json:"nodeUuid"`
}

// PathRebasedPayload is the payload for EVENT_PATH_REBASED.
type PathRebasedPayload struct {
	NodeUUID       string `json:"nodeUuid"`
	TargetFilePath string `json:"targetFilePath"`
	TargetFileName string `json:"targetFileName,omitempty"`
	// TargetStorageLocationID is deprecated and ignored -- see
	// NodeCreatedPayload.StorageLocationID.
	TargetStorageLocationID int64   `json:"targetStorageLocationId,omitempty"`
	MtimeUnix               int64   `json:"mtimeUnix,omitempty"`
	FastHash                *string `json:"fastHash,omitempty"`
	SizeBytes               int64   `json:"sizeBytes,omitempty"`
}

// EventEnvelope is the request body for POST /api/v1/agent/events
// (internal/httpapi.AgentEventInput.Body in branchdam). All three fields are
// required:"true" server-side. Payload is the JSON-string-encoded (i.e.
// double-encoded) form of one of the payload structs above -- sending an
// object here is a 422. The server never validates Payload's contents at
// enqueue time; a malformed payload is accepted (202) and only dies later,
// asynchronously, in the drainer -- with no feedback channel back to the
// agent (plan gap 3).
type EventEnvelope struct {
	AgentID   string `json:"agentId"`
	EventType string `json:"eventType"`
	Payload   string `json:"payload"`
}

// EventResponse is the response body for POST /api/v1/agent/events
// (internal/httpapi.AgentEventOutput.Body in branchdam), returned on 202
// Accepted. 202 means "enqueued," never "applied."
type EventResponse struct {
	EventID string `json:"eventId"`
}

// HelloResponse is the response body for POST /api/v1/agent/hello
// (internal/httpapi.AgentHelloOutput.Body in branchdam). Note the field is
// "version", not "serverVersion" -- HandshakeResponse below uses the latter
// for the same concept. The two are NOT interchangeable; do not share one
// Go struct across both endpoints.
type HelloResponse struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// HandshakeRequest is the request body for POST /api/v1/agent/handshake
// (internal/agent.HandshakeRequest in branchdam). Per the plan's contract
// gap 2, this is NOT a resume mechanism: ClientVersion and
// LastProcessedEventUUID are accepted and never read server-side.
type HandshakeRequest struct {
	AgentID                string `json:"agentId"`
	ClientVersion          string `json:"clientVersion,omitempty"`
	LastProcessedEventUUID string `json:"lastProcessedEventUuid,omitempty"`
}

// HandshakeResponse is the response body for POST /api/v1/agent/handshake
// (internal/agent.HandshakeResponse in branchdam). Field is "serverVersion",
// not "version" (contrast HelloResponse). PendingEventsCount is
// server-global, not scoped to AgentID; AcknowledgedEventUUID only ever
// names a PROCESSED row, so it silently skips any FAILED ones (plan gap 2).
type HandshakeResponse struct {
	OK                    bool   `json:"ok"`
	ServerVersion         string `json:"serverVersion"`
	ServerTimeUnix        int64  `json:"serverTimeUnix"`
	AcknowledgedEventUUID string `json:"acknowledgedEventUuid,omitempty"`
	PendingEventsCount    int64  `json:"pendingEventsCount"`
	NamingTemplate        string `json:"namingTemplate,omitempty"`
}

// UploadOptions configures headers for streaming upload to POST /api/v1/agent/upload.
type UploadOptions struct {
	Filename         string
	CameraModel      string
	CaptureTimestamp int64
	Blake3Hash       string
}

// UploadResponse is the response body for POST /api/v1/agent/upload
// (internal/httpapi.AgentUploadResponse in branchdam), returned on 201 Created.
type UploadResponse struct {
	NodeUUID     string `json:"nodeUuid"`
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytesWritten"`
	Blake3Hash   string `json:"blake3Hash"`
	RelativePath string `json:"relativePath,omitempty"`
}

// RebaseRequest is the request body for POST /api/v1/agent/rebase
// (internal/agent.RebaseRequest in branchdam). Unlike the /events path, this
// call is synchronous: the server resolves the target path against
// storage.Guard immediately and returns either REBASED (known NodeUUID) or
// CREATED (unknown NodeUUID -- the agent is treated as the source of truth
// for an offline-staged file). A read-only, non-TIER3_MASTER_ARCHIVE target
// is refused (400); a TIER3_MASTER_ARCHIVE target is refused (400) unless
// the file already exists there.
type RebaseRequest struct {
	NodeUUID   string  `json:"nodeUuid"`
	TargetPath string  `json:"targetPath"`
	MtimeUnix  int64   `json:"mtimeUnix,omitempty"`
	FileName   string  `json:"fileName,omitempty"`
	FileExt    string  `json:"fileExt,omitempty"`
	SizeBytes  int64   `json:"sizeBytes,omitempty"`
	FastHash   *string `json:"fastHash,omitempty"`
	// FullHash is only consumed server-side on the unknown-NodeUUID (insert)
	// branch -- a known node's rebase never overwrites content hashes, so
	// this only matters for the rare case where this call arrives before its
	// EVENT_NODE_CREATED was applied. Set anyway, since we always have it on
	// hand (queue.Record.FullHash) and it's what unblocks
	// POST /api/v1/agent/node-status ever reporting "verified" for that node.
	FullHash *string `json:"fullHash,omitempty"`
	// StorageLocationID is deprecated and ignored -- see
	// NodeCreatedPayload.StorageLocationID.
	StorageLocationID int64 `json:"storageLocationId,omitempty"`
}

// RebaseResponse is the response body for POST /api/v1/agent/rebase
// (internal/agent.RebaseResponse in branchdam). Status is "REBASED" or
// "CREATED".
type RebaseResponse struct {
	ID                int64  `json:"id"`
	NodeUUID          string `json:"nodeUuid"`
	StorageLocationID int64  `json:"storageLocationId"`
	FilePath          string `json:"filePath"`
	Status            string `json:"status"`
}

// NodeStatusRequest is the request body for POST /api/v1/agent/node-status
// (internal/httpapi.AgentNodeStatusInput in branchdam) -- the first
// agent-reachable read endpoint; every other /api/v1/agent/* route is
// write-oriented. NodeUUIDs is capped server-side at 200 per call.
type NodeStatusRequest struct {
	NodeUUIDs []string `json:"nodeUuids"`
}

// NodeStatusEntry is one NodeUUID's reported status. Found=false is not an
// error -- it means no media_nodes row currently has that node_uuid (never
// synced, or a stale UUID). Verified mirrors branchDAM's own
// ListPrunableNodes eligibility predicate: full_hash non-NULL and 64 hex
// characters (BLAKE3-256) -- LifecycleState/Tier are only meaningful when
// Found is true.
type NodeStatusEntry struct {
	NodeUUID       string `json:"nodeUuid"`
	Found          bool   `json:"found"`
	LifecycleState string `json:"lifecycleState,omitempty"`
	Tier           string `json:"tier,omitempty"`
	Verified       bool   `json:"verified"`
}

// NodeStatusResponse is the response body for POST /api/v1/agent/node-status
// (internal/httpapi.AgentNodeStatusOutput in branchdam).
type NodeStatusResponse struct {
	Statuses []NodeStatusEntry `json:"statuses"`
}

// ContentCheckResult is the response body for GET /api/v1/agent/check-content.
type ContentCheckResult struct {
	Found          bool   `json:"found"`
	NodeUUID       string `json:"nodeUuid,omitempty"`
	FilePath       string `json:"filePath,omitempty"`
	LifecycleState string `json:"lifecycleState,omitempty"`
}
