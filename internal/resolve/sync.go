package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// PathRewrite maps a Windows path prefix to a NAS/container path prefix.
// Longest-prefix matching is used (same convention as internal/ingest/pathmap).
type PathRewrite struct {
	From string // e.g. "D:\\Videos\\"
	To   string // e.g. "/storage/archive/videos/"
}

// Stats summarizes one Sync run.
type Stats struct {
	ClipsFound int // unique file paths found in timelines
	Emitted    int // edges actually posted (or, in a dry run, that would have been)
	Unresolved int // clips whose rewritten path had no node-index entry
	NoRewrite  int // clips whose Windows path matched no PathRewrite rule
	Errors     int // PostEdgeAttached calls that returned an error
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

// Syncer reads a Resolve project database and emits EVENT_EDGE_ATTACHED
// for each unique file path referenced by a timeline.
type Syncer struct {
	DB           *DB
	Index        nodeindex.Resolver
	Client       EdgeAttacher
	AgentID      string
	DatabaseURL  string
	Query        string // defaults to DefaultTimelineQuery if empty
	DryRun       bool
	PathRewrites []PathRewrite
	Logger       *slog.Logger
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Sync reads timeline clips from s.DB, rewrites Windows paths via
// s.PathRewrites, resolves each via s.Index, and emits an
// EVENT_EDGE_ATTACHED for each resolved path.
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
	// edge per unique file path.
	type clipKey struct {
		mediaFilePath string
		timelineName  string
	}
	seen := make(map[clipKey]bool, len(clips))
	var unique []TimelineClip
	for _, clip := range clips {
		k := clipKey{mediaFilePath: clip.MediaFilePath, timelineName: clip.TimelineName}
		if seen[k] {
			continue
		}
		seen[k] = true
		unique = append(unique, clip)
	}

	var stats Stats
	stats.ClipsFound = len(unique)

	for _, clip := range unique {
		rewrittenPath, ok := rewritePath(s.PathRewrites, clip.MediaFilePath)
		if !ok {
			stats.NoRewrite++
			s.logger().Info("resolve-sync: skipping clip, no path rewrite matches",
				"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
			continue
		}

		nodeUUID, nodeOK, err := s.Index.Resolve(rewrittenPath)
		if err != nil {
			return stats, fmt.Errorf("resolve: resolve path %q: %w", rewrittenPath, err)
		}
		if !nodeOK {
			stats.Unresolved++
			s.logger().Info("resolve-sync: skipping clip, path not in node index",
				"mediaFilePath", clip.MediaFilePath, "rewrittenPath", rewrittenPath,
				"timeline", clip.TimelineName)
			continue
		}

		ev := evidence{
			SchemaMapping: SchemaMappingVersion,
			DatabaseURL:   s.DatabaseURL,
			TimelineName:  clip.TimelineName,
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

		// Source = the ingested media file; Target = the same node.
		// The server skips self-edges (targetNodeID == child.ID), so
		// this becomes a metadata-only pass. The evidence JSON is the
		// real value — it records which Resolve timelines reference this
		// file and at which in/out points. Proper PROJECT_SIDECAR edges
		// (media → virtual project node) are a v2 enhancement.
		payload := branchdam.EdgeAttachedPayload{
			SourceNodeUUID:   nodeUUID,
			TargetNodeUUID:   nodeUUID,
			RelationshipType: RelationshipType,
			Confidence:       Confidence,
			Tier:             Tier,
			Resolver:         ResolverName,
			EvidenceJSON:     evJSON,
		}

		if s.DryRun {
			s.logger().Info("resolve-sync: (dry run) would emit edge",
				"nodeUuid", nodeUUID, "mediaFilePath", clip.MediaFilePath,
				"rewrittenPath", rewrittenPath, "timeline", clip.TimelineName)
			stats.Emitted++
			continue
		}

		if _, err := s.Client.PostEdgeAttached(ctx, s.AgentID, payload); err != nil {
			stats.Errors++
			s.logger().Error("resolve-sync: PostEdgeAttached failed",
				"nodeUuid", nodeUUID, "mediaFilePath", clip.MediaFilePath, "err", err)
			continue
		}
		stats.Emitted++
		s.logger().Info("resolve-sync: emitted edge",
			"nodeUuid", nodeUUID, "mediaFilePath", clip.MediaFilePath,
			"rewrittenPath", rewrittenPath, "timeline", clip.TimelineName)
	}

	return stats, nil
}

// rewritePath applies the longest-prefix match from rewrites to windowsPath.
// Returns the rewritten path and true on match, or empty string and false
// when no prefix matches. Backslashes in the input are normalized to forward
// slashes in the output (matching nodeindex.Resolver's verbatim convention).
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
