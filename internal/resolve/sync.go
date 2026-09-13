package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
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
// tests can substitute a fake without a real HTTP server. Currently unused —
// Sync logs evidence instead of emitting edges (server rejects self-edges via
// CHECK constraint). Will be used when proper PROJECT_SIDECAR edges
// (media → virtual project node) land in v2 (issue #184).
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
	ClipsFound   int // unique file paths found in timelines
	Emitted      int // edges actually posted (or, in a dry run, that would have been)
	Unresolved   int // clips whose rewritten path had no node-index entry
	NoRewrite    int // clips whose Windows path matched no PathRewrite rule
	Errors       int // PostEdgeAttached calls that returned an error
	EvidenceOnly int // clips whose evidence was logged but no edge emitted (virtual project node not yet supported)
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

// Syncer reads a Resolve project database and logs evidence metadata for
// each unique file path referenced by a timeline. v2 will emit
// EVENT_EDGE_ATTACHED events once proper PROJECT_SIDECAR edges (media →
// virtual project node) are supported (issue #184).
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
// s.PathRewrites, resolves each via s.Index, and logs evidence metadata.
// In non-dry-run mode, evidence JSON is logged as structured slog output.
// v2 will emit EVENT_EDGE_ATTACHED events via s.Client.
func (s *Syncer) Sync(ctx context.Context) (Stats, error) {
	// Warm the connection before the main query — with SetMaxOpenConns(1),
	// an idle-reaped connection fails on the first query, not at Open time.
	if err := s.DB.db.PingContext(ctx); err != nil {
		return Stats{}, fmt.Errorf("resolve: ping database: %w", err)
	}

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

	var stats Stats
	stats.ClipsFound = len(order)

	// Strip credentials from DatabaseURL before stamping into evidence.
	dbURL := stripCredentials(s.DatabaseURL)

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

		if s.DryRun {
			s.logger().Info("resolve-sync: (dry run) would emit edge",
				"nodeUuid", nodeUUID, "mediaFilePath", clip.MediaFilePath,
				"rewrittenPath", rewrittenPath, "timeline", clip.TimelineName)
			stats.Emitted++
			continue
		}

		// Emit a self-edge (source == target) to attach Resolve metadata.
		// The server has CHECK (source_node_id <> target_node_id) which
		// rejects self-edges, so we log the evidence as structured output
		// instead. The evidence JSON records which Resolve timelines
		// reference this file and at which in/out points — the real value
		// of this integration. Proper PROJECT_SIDECAR edges (media →
		// virtual project node) are a v2 enhancement (issue #184).
		stats.EvidenceOnly++
		s.logger().Info("resolve-sync: resolve evidence",
			"nodeUuid", nodeUUID, "mediaFilePath", clip.MediaFilePath,
			"rewrittenPath", rewrittenPath, "timelines", strings.Join(acc.timelines, ", "),
			"evidence", string(evJSON))
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

// rewritePath applies the longest-prefix match from rewrites to windowsPath.
// Returns the rewritten path and true on match, or empty string and false
// when no prefix matches. Backslashes in the input are normalized to forward
// slashes in the output (matching nodeindex.Resolver's verbatim convention).
//
// Note: rw.From is also normalized at every call. If the operator's YAML
// contains a literal "D:\Videos\" (with a trailing backslash that JSON/YAML
// parses), the length difference will misalign prefix matching. Use a
// trailing forward slash in YAML: "D:\\Videos\\".
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
