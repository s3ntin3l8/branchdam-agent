package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

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

// MembershipEntry is one (media path, timeline ID) pair from a Resolve
// sync pass. Stored in the runtime state file for delta detection across
// sync passes. The agent cannot query which edges exist on the server,
// so this local snapshot is the only way to detect removals.
//
// Duplicated in internal/runtime and internal/tray — kept in sync by
// convention to avoid import cycles between the three packages.
type MembershipEntry struct {
	MediaPath  string `json:"mp"`
	TimelineID string `json:"tl"`
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
	// Delta detection fields (populated when PrevMemberships is set):
	NewMemberships int // memberships in current query but not PrevMemberships
	Unchanged      int // memberships present in both passes (edges skipped)
	Removed        int // memberships in PrevMemberships but not current pass
	FileMissing    int // clips in current query whose rewritten path doesn't exist on disk
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

	// PrevMemberships is the set of (mediaPath, timelineID) pairs from
	// the previous successful sync pass, loaded from the runtime state
	// file. When set, Sync performs delta detection: only newly-added
	// memberships emit edges, unchanged memberships are skipped, and
	// removed memberships are logged. When nil, all memberships are
	// treated as added (first-pass behavior).
	PrevMemberships []MembershipEntry

	// OnSaveMemberships is called after a successful sync with the
	// current pass's membership set. The callback persists the set to
	// the runtime state file for the next pass's delta detection.
	// Called after r.mu.Unlock() semantics: slow saves must not block
	// the sync pass's return. Nil means no persistence (dry run or test).
	OnSaveMemberships func(entries []MembershipEntry) error
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// VirtualNodeUUID generates a deterministic UUID for a virtual project node
// from the agent ID, stable database timeline ID, and credential-free database
// identity. Agent scope is required because branchDAM enforces live paths
// globally: two workstations syncing the same shared Resolve database must not
// claim the same virtual node. Password rotation therefore cannot create a
// second node, while separate agents and same-named timelines remain distinct.
func VirtualNodeUUID(agentID, timelineID, databaseURL string) string {
	h := sha256.Sum256([]byte(agentID + "\x00" + timelineID + "\x00" + databaseIdentity(databaseURL)))
	// UUID v4 from first 16 bytes of SHA-256, set version and variant bits.
	u := h[:16]
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// virtualFilePath returns a collision-free virtual path for a project node.
func virtualFilePath(virtualRoot, nodeUUID string) string {
	return path.Clean(virtualRoot + "/" + nodeUUID)
}

// virtualDisplayName returns a human-readable label for a virtual project node.
func virtualDisplayName(timelineName string) string {
	return "Resolve: " + timelineName
}

// resolveAgentID returns a stable per-install identifier for the Resolve
// integration. It prefers the configured ID, then the OS machine ID, then a
// generated UUID persisted below the user's config directory. Hostnames are
// deliberately excluded because cloned machines can share them.
func resolveAgentID(cfgAgentID, machineIDPath, persistentIDPath string, logger *slog.Logger) (string, error) {
	if cfgAgentID != "" {
		return cfgAgentID, nil
	}
	if b, err := os.ReadFile(machineIDPath); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	}
	if b, err := os.ReadFile(persistentIDPath); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id, nil
		}
	}
	newID := uuid.New().String()
	if err := os.MkdirAll(filepath.Dir(persistentIDPath), 0o755); err != nil {
		return "", fmt.Errorf("resolve: create persistent-id dir: %w", err)
	}
	if err := os.WriteFile(persistentIDPath, []byte(newID+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("resolve: write persistent-id file: %w", err)
	}
	if logger != nil {
		logger.Warn("resolve: agentId empty; minted per-install ID; set agentId explicitly for shared-workstation deployments",
			"persistentIDPath", persistentIDPath)
	}
	return newID, nil
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
	if s.AgentID == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return Stats{}, fmt.Errorf("resolve: config dir unavailable and agentId not configured: %w", err)
		}
		persistentIDPath := filepath.Join(configDir, "branchdam-agent", "resolve-install-id")
		id, err := resolveAgentID("", "/etc/machine-id", persistentIDPath, s.logger())
		if err != nil {
			return Stats{}, err
		}
		s.AgentID = id
	}

	clips, err := s.DB.TimelineClips(ctx, query)
	if err != nil {
		return Stats{}, fmt.Errorf("resolve: read timeline clips: %w", err)
	}

	// Deduplicate repeated uses within one timeline, but retain a membership
	// for every distinct (media path, timeline ID) pair. One source clip used
	// in two timelines must produce two PROJECT_SIDECAR edges.
	type membershipKey struct {
		mediaPath  string
		timelineID string
	}
	memberships := make(map[membershipKey]TimelineClip, len(clips))
	var membershipOrder []membershipKey
	uniquePaths := make(map[string]struct{}, len(clips))

	// Collect unique timelines by their stable database ID, never display
	// name (Resolve permits multiple timelines with the same name).
	type timelineInfo struct {
		name string
		id   string
	}
	timelineSeen := make(map[string]bool)
	var timelines []timelineInfo
	for _, clip := range clips {
		if strings.TrimSpace(clip.TimelineID) == "" {
			return Stats{}, fmt.Errorf("resolve: timeline clip %q has empty timeline ID", clip.ClipName)
		}
		uniquePaths[clip.MediaFilePath] = struct{}{}
		key := membershipKey{mediaPath: clip.MediaFilePath, timelineID: clip.TimelineID}
		if _, ok := memberships[key]; !ok {
			memberships[key] = clip
			membershipOrder = append(membershipOrder, key)
		}
		if !timelineSeen[clip.TimelineID] {
			timelineSeen[clip.TimelineID] = true
			timelines = append(timelines, timelineInfo{name: clip.TimelineName, id: clip.TimelineID})
		}
	}

	var stats Stats
	stats.ClipsFound = len(uniquePaths)

	virtualRoot := s.VirtualRoot
	if virtualRoot == "" {
		virtualRoot = "/virtual/resolve"
	}

	dbURL := schemeOnly(s.DatabaseURL)

	// Create virtual project nodes for each unique timeline.
	timelineUUIDs := make(map[string]string) // timeline ID → virtual node UUID
	for _, tl := range timelines {
		nodeUUID := VirtualNodeUUID(s.AgentID, tl.id, s.DatabaseURL)

		fp := virtualFilePath(virtualRoot, nodeUUID)
		displayName := virtualDisplayName(tl.name)

		if s.DryRun {
			timelineUUIDs[tl.id] = nodeUUID
			s.logger().Info("resolve-sync: (dry run) would create virtual node",
				"nodeUuid", nodeUUID, "filePath", fp, "timeline", tl.name)
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
			EvidenceJSON: func() json.RawMessage {
				timelineEvidence := struct {
					SchemaMapping string `json:"schemaMapping"`
					DatabaseURL   string `json:"databaseUrl"`
					TimelineName  string `json:"timelineName"`
				}{
					SchemaMapping: SchemaMappingVersion,
					DatabaseURL:   dbURL,
					TimelineName:  tl.name,
				}
				b, _ := json.Marshal(timelineEvidence)
				return b
			}(),
		})
		if err != nil {
			stats.Errors++
			s.logger().Error("resolve-sync: failed to create virtual node",
				"timeline", tl.name, "nodeUuid", nodeUUID, "err", err)
			continue
		}
		stats.VirtualNodes++
		timelineUUIDs[tl.id] = nodeUUID
		s.logger().Info("resolve-sync: created virtual node",
			"nodeUuid", nodeUUID, "filePath", fp, "timeline", tl.name)
	}

	// Emit PROJECT_SIDECAR edges from each clip to its timeline's virtual node.
	// When PrevMemberships is set, only newly-added memberships emit edges;
	// unchanged memberships are skipped and removed memberships are logged.
	prevSet := make(map[membershipKey]struct{}, len(s.PrevMemberships))
	for _, m := range s.PrevMemberships {
		prevSet[membershipKey{mediaPath: m.MediaPath, timelineID: m.TimelineID}] = struct{}{}
	}
	hasPrev := s.PrevMemberships != nil

	// Track the next-pass baseline separately from emitted clips. The
	// next baseline MUST include both newly-emitted clips AND unchanged
	// clips (consumed from prevSet) -- otherwise an all-unchanged pass
	// would emit nothing and OnSaveMemberships would receive an empty
	// set, resetting s.prevMemberships to nil on the next call. The next
	// pass would then treat every clip as new and re-emit, defeating
	// delta detection for the entire session after the first no-change
	// pass. (Errored, unresolved, and no-rewrite clips are deliberately
	// excluded -- they should be retried on the next pass, not pinned
	// in the baseline as "still here".)
	var emittedMemberships, nextBaseline []MembershipEntry

	for _, key := range membershipOrder {
		clip := memberships[key]

		rewrittenPath, ok := rewritePath(s.PathRewrites, clip.MediaFilePath)
		if !ok {
			stats.NoRewrite++
			// Consume from prevSet so no-rewrite clips aren't
			// falsely reported as "removed from timeline" on the
			// next pass. They simply can't be emitted; not
			// persisted, so next pass retries them as new.
			if hasPrev {
				delete(prevSet, key)
			}
			s.logger().Info("resolve-sync: skipping clip, no path rewrite matches",
				"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
			continue
		}

		// File-existence check: detect clips whose referenced file is
		// missing from disk (asset moved/deleted but still in timeline).
		if _, lstatErr := os.Lstat(rewrittenPath); lstatErr != nil && errors.Is(lstatErr, fs.ErrNotExist) {
			stats.FileMissing++
			s.logger().Warn("resolve-sync: clip file missing from disk",
				"mediaFilePath", clip.MediaFilePath, "rewrittenPath", rewrittenPath,
				"timeline", clip.TimelineName)
		}

		// Delta detection: classify this membership as added or unchanged.
		if hasPrev {
			if _, existed := prevSet[key]; existed {
				delete(prevSet, key) // consumed
				stats.Unchanged++
				// Unchanged clips MUST stay in the next baseline so a
				// subsequent all-unchanged pass doesn't reset it to
				// empty (which would re-emit everything as new).
				nextBaseline = append(nextBaseline, MembershipEntry{
					MediaPath:  key.mediaPath,
					TimelineID: key.timelineID,
				})
				continue // skip server call — edge already emitted
			}
		}
		stats.NewMemberships++

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

		// Build evidence for this particular timeline membership.
		ev := evidence{
			SchemaMapping: SchemaMappingVersion,
			DatabaseURL:   dbURL,
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

		targetUUID := timelineUUIDs[clip.TimelineID]
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
			// DryRun: count as emitted for stats but don't persist —
			// the edge wasn't actually posted, so persisting would
			// mark it "unchanged" on the next real pass.
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
		stats.Emitted++
		stats.EdgesAttached++
		entry := MembershipEntry{
			MediaPath:  clip.MediaFilePath,
			TimelineID: clip.TimelineID,
		}
		emittedMemberships = append(emittedMemberships, entry)
		// Newly-emitted clips obviously belong in the next baseline.
		nextBaseline = append(nextBaseline, entry)
		s.logger().Info("resolve-sync: emitted edge",
			"sourceUuid", nodeUUID, "targetUuid", targetUUID,
			"mediaFilePath", clip.MediaFilePath, "timeline", clip.TimelineName)
	}

	// Detect removed memberships: entries in PrevMemberships that are not
	// in the current pass's query result. The agent cannot query which
	// edges exist on the server, so this is informational only — no
	// server-side edge deletion occurs. When a path rewrite matches, the
	// file-existence check distinguishes intentional removals (file still
	// on disk) from missing-asset errors (file also gone).
	if hasPrev {
		for key := range prevSet {
			stats.Removed++
			rewrittenPath, ok := rewritePath(s.PathRewrites, key.mediaPath)
			if ok {
				if _, lstatErr := os.Lstat(rewrittenPath); lstatErr != nil && errors.Is(lstatErr, fs.ErrNotExist) {
					stats.FileMissing++
					s.logger().Warn("resolve-sync: removed clip file also missing from disk",
						"mediaPath", key.mediaPath, "rewrittenPath", rewrittenPath, "timelineId", key.timelineID)
					continue
				}
			}
			// Non-ErrNotExist Lstat errors (permission denied, etc.) are
			// deliberately ignored — the file may be inaccessible but not
			// missing.
			s.logger().Info("resolve-sync: clip removed from timeline since last sync",
				"mediaPath", key.mediaPath, "timelineId", key.timelineID)
		}
	}

	// Persist the next-pass baseline: clips that successfully emitted
	// (new this pass) AND unchanged clips (consumed from prevSet). This
	// keeps the in-session baseline advancing across passes -- an
	// all-unchanged pass would emit nothing, but the baseline still
	// reflects what exists, so the next pass correctly classifies
	// everything as Unchanged and skips re-emission. Errored,
	// unresolved, and no-rewrite clips are deliberately excluded -- they
	// should be retried on the next pass, not pinned in the baseline.
	if !s.DryRun && s.OnSaveMemberships != nil {
		if err := s.OnSaveMemberships(nextBaseline); err != nil {
			s.logger().Warn("resolve-sync: failed to persist membership set", "err", err)
		}
	}

	return stats, nil
}

// databaseIdentity removes credentials and connection-only query parameters
// from a DSN before it participates in persistent virtual-node identity.
// Host/path (or a file URL's opaque path) still distinguish databases.
func databaseIdentity(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// stripCredentials removes userinfo from a database URL so it can be
// safely included in evidence JSON persisted server-side. Returns the
// original string if parsing fails or no credentials are present.
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
