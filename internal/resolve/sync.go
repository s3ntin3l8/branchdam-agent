package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/nodeindex"
)

// SchemaMappingVersion is stamped into every emitted edge's evidenceJson.
// Bump this whenever DefaultTimelineQuery changes in a way that could
// change which clips get emitted — it is what lets a future data-correction
// migration find every edge a particular schema-mapping version produced.
const SchemaMappingVersion = "resolve-projectdb-1"

// Tier, Confidence, and Resolver are the plan's values for a Resolve-sourced
// edge — Tier 1 (Deterministic), Confidence 1.00 (the database is the
// source of truth for which files a timeline references).
const (
	Tier             = 1
	Confidence       = 1.00
	RelationshipType = branchdam.RelationshipProjectSidecar
	ResolverName     = "resolve_project_db"
)

// EdgeAttacher is the subset of *branchdam.Client's surface Syncer needs, so
// tests can substitute a fake without a real HTTP server.
type EdgeAttacher interface {
	PostEdgeAttached(ctx context.Context, agentID string, payload branchdam.EdgeAttachedPayload) (*branchdam.EventResponse, error)
}

// VirtualNodeEmitter is the subset of *branchdam.Client's surface needed to
// create virtual project nodes for integration timelines.
type VirtualNodeEmitter interface {
	PostVirtualNodeCreated(ctx context.Context, agentID string, payload branchdam.VirtualNodeCreated) (*branchdam.EventResponse, error)
}

// PathRewrite maps a Windows path prefix to a NAS/container path prefix.
// Longest-prefix matching is used (same convention as internal/ingest/pathmap).
type PathRewrite struct {
	From string // e.g. "D:\\Videos\\"
	To   string // e.g. "/storage/archive/videos/"
}

// Stats summarizes one Sync run.
type Stats struct {
	ClipsFound    int // unique file paths found in timelines
	Emitted       int // edges actually posted (or, in a dry run, that would have been)
	Unresolved    int // clips whose rewritten path had no node-index entry
	NoRewrite     int // clips whose Windows path matched no PathRewrite rule
	Errors        int // PostEdgeAttached calls that returned an error
	VirtualNodes  int // virtual project nodes created
	EdgesAttached int // PROJECT_SIDECAR edges emitted
	EvidenceOnly  int // clips whose evidence was logged but no edge emitted (dry run mode)
}

// evidence is the evidenceJson object stamped onto every emitted edge.
type evidence struct {
	SchemaMapping string `json:"schemaMapping"`
	DatabaseURL   string `json:"databaseUrl"`
	TimelineName  string `json:"timelineName"`
	ClipName      string `json:"clipName"`
	MediaFilePath string `json:"mediaFilePath"`
	RewrittenPath string `json:"rewrittenPath"`
	InPoint       string `json:"inPoint,omitempty"`
	StartFrame    string `json:"startFrame,omitempty"`
	Duration      string `json:"duration,omitempty"`
	ItemID        string `json:"itemId"`
	TimelineID    string `json:"timelineId"`
}

// Syncer reads a Resolve project database and emits EVENT_VIRTUAL_NODE_CREATED
// events for each unique timeline, then EVENT_EDGE_ATTACHED events
// (PROJECT_SIDECAR) linking media clips to their virtual project nodes.
type Syncer struct {
	DB             *DB
	Index          nodeindex.Resolver
	Client         EdgeAttacher
	VirtualEmitter VirtualNodeEmitter
	AgentID        string
	DatabaseURL    string
	Query          string // defaults to DefaultTimelineQuery if empty
	DryRun         bool
	PathRewrites   []PathRewrite
	VirtualRoot    string // root prefix for virtual paths, e.g. "/virtual/resolve"
	Logger         *slog.Logger
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// VirtualNodeUUID generates a deterministic UUID for a virtual project node
// from the agent ID, timeline name, and database URL. The same inputs always
// produce the same UUID, so re-syncs are idempotent — the server's "already
// exists" check makes re-sends safe. agentID is included to avoid collisions
// when multiple workstations edit timelines with the same name.
func VirtualNodeUUID(agentID, timelineName, databaseURL string) string {
	h := sha256.Sum256([]byte(agentID + "\x00" + timelineName + "\x00" + databaseURL))
	// UUID v4 from first 16 bytes of SHA-256, set version and variant bits.
	u := h[:16]
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// sanitizeTimelineName makes a timeline name safe for use as a path component.
// Resolve timeline names are untrusted DB text — a name like ".." would escape
// the virtual root, and "/" would build arbitrary nested paths. This function
// replaces path separators and dot-only names with safe alternatives.
func sanitizeTimelineName(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, "..", "_")
	name = strings.Trim(name, ".")
	if name == "" || name == "." || name == ".." {
		name = "unnamed-timeline"
	}
	return name
}

// virtualFilePath returns the virtual path for a timeline's project node.
// The path is scoped per agent to avoid ux_media_nodes_live_path collisions
// across workstations.
func virtualFilePath(virtualRoot, agentID, timelineName string) string {
	return path.Clean(virtualRoot + "/" + agentID + "/" + timelineName)
}

// virtualDisplayName returns a human-readable label for a virtual project node.
func virtualDisplayName(timelineName string) string {
	return "Resolve: " + timelineName
}

// Sync reads timeline clips from s.DB, rewrites Windows paths via
// s.PathRewrites, resolves each via s.Index, and emits virtual project nodes
// and PROJECT_SIDECAR edges. In dry-run mode, it logs what would be emitted
// without calling the server.
func (s *Syncer) Sync(ctx context.Context) (Stats, error) {
	query := s.Query
	if query == "" {
		query = DefaultTimelineQuery
	}

	clips, err := s.DB.TimelineClips(ctx, query)
	if err != nil {
		return Stats{}, fmt.Errorf("resolve: read timeline clips: %w", err)
	}

	// Deduplicate by MediaFilePath — the same file may appear in multiple
	// timelines or multiple times in the same timeline. We only need one
	// edge per unique file path; evidence.TimelineNames collects all
	// timelines that reference the file.
	type clipAccum struct {
		clip          TimelineClip
		timelines     []string
		seenTimelines map[string]bool
	}
	byPath := make(map[string]*clipAccum, len(clips))
	var order []string // preserve first-seen order
	for _, clip := range clips {
		acc, ok := byPath[clip.MediaFilePath]
		if !ok {
			acc = &clipAccum{
				clip:          clip,
				seenTimelines: make(map[string]bool),
			}
			byPath[clip.MediaFilePath] = acc
			order = append(order, clip.MediaFilePath)
		}
		if !acc.seenTimelines[clip.TimelineName] {
			acc.seenTimelines[clip.TimelineName] = true
			acc.timelines = append(acc.timelines, clip.TimelineName)
		}
	}

	// Collect unique timelines seen across all clips. We need one virtual
	// project node per timeline. Timeline names from the Resolve DB are
	// untrusted — sanitize for safe use as path components.
	type timelineInfo struct {
		name string
		id   string
	}
	timelineSeen := make(map[string]bool)
	var timelines []timelineInfo
	for _, path := range order {
		for _, tl := range byPath[path].timelines {
			safe := sanitizeTimelineName(tl)
			if !timelineSeen[safe] {
				timelineSeen[safe] = true
				// Find the timeline_id for this timeline name from the clips.
				for _, clip := range clips {
					if clip.TimelineName == tl {
						timelines = append(timelines, timelineInfo{name: safe, id: clip.TimelineID})
						break
					}
				}
			}
		}
	}

	var stats Stats
	stats.ClipsFound = len(order)

	virtualRoot := s.VirtualRoot
	if virtualRoot == "" {
		virtualRoot = "/virtual/resolve"
	}

	dbURL := schemeOnly(s.DatabaseURL)

	// Create virtual project nodes for each unique timeline.
	timelineUUIDs := make(map[string]string) // timelineName → virtual node UUID
	for _, tl := range timelines {
		nodeUUID := VirtualNodeUUID(s.AgentID, tl.name, s.DatabaseURL)

		fp := virtualFilePath(virtualRoot, s.AgentID, tl.name)
		displayName := virtualDisplayName(tl.name)

		if s.DryRun {
			s.logger().Info("resolve-sync: (dry run) would create virtual node",
				"nodeUuid", nodeUUID, "filePath", fp, "timeline", tl.name)
			timelineUUIDs[tl.name] = nodeUUID
			stats.VirtualNodes++
			continue
		}

		if s.VirtualEmitter == nil {
			s.logger().Warn("resolve-sync: no virtual emitter, skipping virtual node creation",
				"timeline", tl.name)
			continue
		}

		_, err := s.VirtualEmitter.PostVirtualNodeCreated(ctx, s.AgentID, branchdam.VirtualNodeCreated{
			NodeUUID:    nodeUUID,
			FilePath:    fp,
			DisplayName: displayName,
			ProjectType: "resolve_project",
		})
		if err != nil {
			stats.Errors++
			s.logger().Error("resolve-sync: failed to create virtual node",
				"timeline", tl.name, "nodeUuid", nodeUUID, "err", err)
			continue
		}
		// Register UUID only after successful creation — the edge loop
		// skips timelines with no UUID, so a failed create means no
		// dangling PROJECT_SIDECAR edges targeting an absent node.
		timelineUUIDs[tl.name] = nodeUUID
		stats.VirtualNodes++
		s.logger().Info("resolve-sync: created virtual node",
			"nodeUuid", nodeUUID, "filePath", fp, "timeline", tl.name)
	}

	// Emit PROJECT_SIDECAR edges from each clip to its timeline's virtual node.
	for _, path := range order {
		acc := byPath[path]
		clip := acc.clip

		rewrittenPath, ok := rewritePath(s.PathRewrites, clip.MediaFilePath)
		if !ok {
			stats.NoRewrite++
			s.logger().Info("resolve-sync: skipping clip, no path rewrite matches",
				"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
			continue
		}

		nodeUUID, nodeOK, err := s.Index.Resolve(rewrittenPath)
		if err != nil {
			stats.Errors++
			s.logger().Error("resolve-sync: resolve path failed, skipping",
				"rewrittenPath", rewrittenPath, "err", err)
			continue
		}
		if !nodeOK {
			stats.Unresolved++
			s.logger().Info("resolve-sync: skipping clip, path not in node index",
				"mediaFilePath", clip.MediaFilePath, "rewrittenPath", rewrittenPath,
				"timeline", clip.TimelineName)
			continue
		}

		// Build evidence with all timelines that reference this file.
		ev := evidence{
			SchemaMapping: SchemaMappingVersion,
			DatabaseURL:   dbURL,
			TimelineName:  strings.Join(acc.timelines, ", "),
			ClipName:      clip.ClipName,
			MediaFilePath: clip.MediaFilePath,
			RewrittenPath: rewrittenPath,
			InPoint:       clip.InPoint.String,
			StartFrame:    clip.StartFrame.String,
			Duration:      clip.Duration.String,
			ItemID:        clip.ItemID,
			TimelineID:    clip.TimelineID,
		}
		evJSON, err := json.Marshal(ev)
		if err != nil {
			return stats, fmt.Errorf("resolve: marshal evidence for %q: %w", clip.MediaFilePath, err)
		}

		targetUUID := timelineUUIDs[sanitizeTimelineName(clip.TimelineName)]
		if targetUUID == "" {
			stats.Errors++
			s.logger().Error("resolve-sync: no virtual node UUID for timeline",
				"timeline", clip.TimelineName)
			continue
		}

		if s.DryRun {
			s.logger().Info("resolve-sync: (dry run) would emit edge",
				"sourceUuid", nodeUUID, "targetUuid", targetUUID,
				"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
			stats.Emitted++
			continue
		}

		if s.Client == nil {
			s.logger().Warn("resolve-sync: no edge emitter, skipping edge",
				"timeline", clip.TimelineName)
			continue
		}

		_, err = s.Client.PostEdgeAttached(ctx, s.AgentID, branchdam.EdgeAttachedPayload{
			SourceNodeUUID:   nodeUUID,
			TargetNodeUUID:   targetUUID,
			RelationshipType: RelationshipType,
			Confidence:       Confidence,
			Tier:             Tier,
			Resolver:         ResolverName,
			EvidenceJSON:     evJSON,
		})
		if err != nil {
			stats.Errors++
			s.logger().Error("resolve-sync: failed to emit edge",
				"sourceUuid", nodeUUID, "targetUuid", targetUUID, "err", err)
			continue
		}
		stats.EdgesAttached++
		s.logger().Info("resolve-sync: emitted edge",
			"sourceUuid", nodeUUID, "targetUuid", targetUUID,
			"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
	}

	return stats, nil
}

// stripCredentials removes userinfo from a database URL so it can be safely
// included in evidence JSON persisted server-side. Returns the original
// string if parsing fails or no credentials are present.
func stripCredentials(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String()
}

// schemeOnly returns just the scheme portion of a database URL
// (e.g. "postgres", "file") for minimal evidence stamping.
// Returns the original string if parsing fails.
func schemeOnly(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Scheme
}

// rewritePath applies the longest-prefix match from rewrites to windowsPath.
// Returns the rewritten path and true on match, or empty string and false
// when no prefix matches. Backslashes in the input are normalized to forward
// slashes in the output (matching nodeindex.Resolver's verbatim convention).
//
// YAML snippet: D:\\Videos\\ (double-quoted, escaped backslash) and
// 'D:\Videos\' (single-quoted, literal backslash) both yield the same
// in-memory string D:\Videos\ — either form works. The trailing backslash
// is what makes the prefix match correctly (e.g. D:\Videos\ matches
// D:\Videos\file.mp4 but not D:\VideosExtra\file.mp4).
func rewritePath(rewrites []PathRewrite, windowsPath string) (string, bool) {
	// Normalize backslashes to forward slashes (Windows paths use backslashes,
	// but nodeindex and branchdam use forward slashes).
	normalized := strings.ReplaceAll(windowsPath, "\\", "/")
	bestPrefixLen := -1
	bestRewrite := PathRewrite{}
	for _, rw := range rewrites {
		from := strings.ReplaceAll(rw.From, "\\", "/")
		if !strings.HasPrefix(normalized, from) {
			continue
		}
		if len(from) > bestPrefixLen {
			bestPrefixLen = len(from)
			bestRewrite = rw
		}
	}
	if bestPrefixLen < 0 {
		return "", false
	}
	// Replace the matched prefix with the target.
	rest := normalized[bestPrefixLen:]
	to := strings.ReplaceAll(bestRewrite.To, "\\", "/")
	rewritten := strings.TrimRight(to, "/") + "/" + rest
	rewritten = filepath.Clean(rewritten)
	return filepath.ToSlash(rewritten), true
}
