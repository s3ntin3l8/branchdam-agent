package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

type snapshotMembershipKey struct {
	path       string
	timelineID string
}

type snapshotPlacement struct {
	ItemID     string `json:"itemId"`
	ClipName   string `json:"clipName"`
	InPoint    string `json:"inPoint,omitempty"`
	StartFrame string `json:"startFrame,omitempty"`
	Duration   string `json:"duration,omitempty"`
}

type snapshotEvidence struct {
	SchemaMapping  string              `json:"schemaMapping"`
	DatabaseURL    string              `json:"databaseUrl"`
	TimelineID     string              `json:"timelineId"`
	TimelineName   string              `json:"timelineName"`
	MediaFilePath  string              `json:"mediaFilePath"`
	RewrittenPath  string              `json:"rewrittenPath"`
	MediaFilePaths []string            `json:"mediaFilePaths,omitempty"`
	RewrittenPaths []string            `json:"rewrittenPaths,omitempty"`
	Placements     []snapshotPlacement `json:"placements"`
}

type resolvedSnapshotKey struct {
	timelineID     string
	sourceNodeUUID string
}

type resolvedSnapshotMembership struct {
	mediaFilePaths []string
	rewrittenPaths []string
	placements     []snapshotPlacement
}

// ScopeID is a credential-free, stable hash of the database identity used
// for virtual timeline UUIDs. The raw local path or server host is never
// sent as the server-side ownership key.
func ScopeID(databaseURL string) string {
	h := sha256.Sum256([]byte(databaseIdentity(databaseURL)))
	return hex.EncodeToString(h[:])
}

// syncSnapshot submits a complete database read. No baseline is advanced on
// transport failure or a server-side rollback; the next pass retries the
// same authoritative snapshot.
func (s *Syncer) syncSnapshot(ctx context.Context, clips []TimelineClip) (Stats, error) {
	groups := make(map[snapshotMembershipKey][]TimelineClip, len(clips))
	timelineNames := make(map[string]string)
	uniquePaths := make(map[string]bool)
	for _, clip := range clips {
		if clip.TimelineID == "" || clip.ItemID == "" {
			return Stats{}, fmt.Errorf("resolve: empty timeline/item ID in database row %q", clip.MediaFilePath)
		}
		if name, ok := timelineNames[clip.TimelineID]; ok && name != clip.TimelineName {
			return Stats{}, fmt.Errorf("resolve: inconsistent timeline name for ID %q", clip.TimelineID)
		}
		timelineNames[clip.TimelineID] = clip.TimelineName
		uniquePaths[clip.MediaFilePath] = true
		key := snapshotMembershipKey{path: clip.MediaFilePath, timelineID: clip.TimelineID}
		groups[key] = append(groups[key], clip)
	}
	stats := Stats{ClipsFound: len(uniquePaths), VirtualNodes: len(timelineNames)}
	virtualRoot := s.VirtualRoot
	if virtualRoot == "" {
		virtualRoot = "/virtual/resolve"
	}
	scopeID := ScopeID(s.DatabaseURL)
	snapshot := branchdam.ResolveSnapshot{AgentID: s.AgentID, ScopeID: scopeID,
		Timelines:               make([]branchdam.ResolveSnapshotTimeline, 0, len(timelineNames)),
		Memberships:             make([]branchdam.ResolveSnapshotMembership, 0, len(groups)),
		LegacyTimelineNodeUUIDs: append([]string(nil), s.LegacyTimelineNodeUUIDs...),
		RetireScopeID:           s.RetireScopeID,
	}
	legacySeen := make(map[string]bool)
	for _, id := range snapshot.LegacyTimelineNodeUUIDs {
		legacySeen[id] = true
	}
	for _, timelineID := range s.LegacyTimelineIDs {
		id := VirtualNodeUUID(s.AgentID, timelineID, s.DatabaseURL)
		if !legacySeen[id] {
			snapshot.LegacyTimelineNodeUUIDs = append(snapshot.LegacyTimelineNodeUUIDs, id)
			legacySeen[id] = true
		}
	}
	sort.Strings(snapshot.LegacyTimelineNodeUUIDs)
	ids := make([]string, 0, len(timelineNames))
	for id := range timelineNames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		nodeUUID := VirtualNodeUUID(s.AgentID, id, s.DatabaseURL)
		ev, err := json.Marshal(struct {
			SchemaMapping string `json:"schemaMapping"`
			DatabaseURL   string `json:"databaseUrl"`
			TimelineName  string `json:"timelineName"`
			TimelineID    string `json:"timelineId"`
		}{SchemaMappingVersion, schemeOnly(s.DatabaseURL), timelineNames[id], id})
		if err != nil {
			return stats, err
		}
		snapshot.Timelines = append(snapshot.Timelines, branchdam.ResolveSnapshotTimeline{
			TimelineID: id, NodeUUID: nodeUUID,
			FilePath:    virtualFilePath(virtualRoot, nodeUUID),
			DisplayName: virtualDisplayName(timelineNames[id]), EvidenceJSON: ev,
		})
	}
	keys := make([]snapshotMembershipKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].timelineID != keys[j].timelineID {
			return keys[i].timelineID < keys[j].timelineID
		}
		return keys[i].path < keys[j].path
	})
	resolved := make(map[resolvedSnapshotKey]*resolvedSnapshotMembership)
	for _, key := range keys {
		member := branchdam.ResolveSnapshotMembership{TimelineID: key.timelineID, MediaFilePath: key.path}
		// Resolve's original path is a workstation-native path only on
		// the OS where filepath.IsAbs recognizes it. The rewritten path
		// is container-absolute and must never be Lstat'd locally.
		if filepath.IsAbs(key.path) {
			if _, err := os.Lstat(key.path); errors.Is(err, fs.ErrNotExist) {
				stats.FileMissing++
				s.logger().Warn("resolve-sync: original clip path missing on workstation", "mediaFilePath", key.path)
			}
		}
		rewritten, ok := rewritePath(s.PathRewrites, key.path)
		if !ok {
			stats.NoRewrite++
			snapshot.Memberships = append(snapshot.Memberships, member)
			continue
		}
		if s.Index == nil {
			stats.Unresolved++
			snapshot.Memberships = append(snapshot.Memberships, member)
			continue
		}
		nodeUUID, found, err := s.Index.Resolve(rewritten)
		if err != nil {
			return stats, fmt.Errorf("resolve: node index lookup %q: %w", rewritten, err)
		}
		if !found {
			stats.Unresolved++
			snapshot.Memberships = append(snapshot.Memberships, member)
			continue
		}
		resolvedKey := resolvedSnapshotKey{timelineID: key.timelineID, sourceNodeUUID: nodeUUID}
		group := resolved[resolvedKey]
		if group == nil {
			group = &resolvedSnapshotMembership{}
			resolved[resolvedKey] = group
		}
		group.mediaFilePaths = append(group.mediaFilePaths, key.path)
		group.rewrittenPaths = append(group.rewrittenPaths, rewritten)
		for _, clip := range groups[key] {
			group.placements = append(group.placements, snapshotPlacement{
				ItemID: clip.ItemID, ClipName: clip.ClipName,
				InPoint: clip.InPoint.String, StartFrame: clip.StartFrame.String,
				Duration: clip.Duration.String,
			})
		}
	}
	resolvedKeys := make([]resolvedSnapshotKey, 0, len(resolved))
	for key := range resolved {
		resolvedKeys = append(resolvedKeys, key)
	}
	sort.Slice(resolvedKeys, func(i, j int) bool {
		if resolvedKeys[i].timelineID != resolvedKeys[j].timelineID {
			return resolvedKeys[i].timelineID < resolvedKeys[j].timelineID
		}
		return resolvedKeys[i].sourceNodeUUID < resolvedKeys[j].sourceNodeUUID
	})
	for _, key := range resolvedKeys {
		group := resolved[key]
		// The source groups were traversed in timeline/path order, so aliases
		// are already deterministic and each rewritten path remains paired by
		// index with its workstation-native media path.
		sort.Slice(group.placements, func(i, j int) bool {
			a, b := group.placements[i], group.placements[j]
			return a.ItemID+"\x00"+a.ClipName+"\x00"+a.InPoint+"\x00"+a.StartFrame+"\x00"+a.Duration <
				b.ItemID+"\x00"+b.ClipName+"\x00"+b.InPoint+"\x00"+b.StartFrame+"\x00"+b.Duration
		})
		member := branchdam.ResolveSnapshotMembership{
			TimelineID: key.timelineID, MediaFilePath: group.mediaFilePaths[0], SourceNodeUUID: key.sourceNodeUUID,
		}
		ev, err := json.Marshal(snapshotEvidence{
			SchemaMapping: SchemaMappingVersion, DatabaseURL: schemeOnly(s.DatabaseURL),
			TimelineID: key.timelineID, TimelineName: timelineNames[key.timelineID],
			MediaFilePath: group.mediaFilePaths[0], RewrittenPath: group.rewrittenPaths[0],
			MediaFilePaths: group.mediaFilePaths, RewrittenPaths: group.rewrittenPaths,
			Placements: group.placements,
		})
		if err != nil {
			return stats, err
		}
		member.EvidenceJSON = ev
		snapshot.Memberships = append(snapshot.Memberships, member)
	}
	sort.Slice(snapshot.Memberships, func(i, j int) bool {
		if snapshot.Memberships[i].TimelineID != snapshot.Memberships[j].TimelineID {
			return snapshot.Memberships[i].TimelineID < snapshot.Memberships[j].TimelineID
		}
		if snapshot.Memberships[i].MediaFilePath != snapshot.Memberships[j].MediaFilePath {
			return snapshot.Memberships[i].MediaFilePath < snapshot.Memberships[j].MediaFilePath
		}
		return snapshot.Memberships[i].SourceNodeUUID < snapshot.Memberships[j].SourceNodeUUID
	})
	if s.DryRun {
		stats.Emitted = len(snapshot.Memberships) - stats.Unresolved - stats.NoRewrite
		stats.EvidenceOnly = stats.Emitted
		s.logger().Info("resolve-sync: dry-run snapshot", "scopeId", scopeID,
			"timelines", len(snapshot.Timelines), "memberships", len(snapshot.Memberships),
			"resolvable", stats.Emitted)
		return stats, nil
	}
	if s.SnapshotClient == nil {
		return stats, fmt.Errorf("resolve: snapshot client unavailable")
	}
	response, err := s.SnapshotClient.PostResolveSnapshot(ctx, snapshot)
	if err != nil {
		return stats, fmt.Errorf("resolve: synchronous snapshot reconciliation: %w", err)
	}
	stats.EdgesAttached = response.Created
	stats.Refreshed = response.Refreshed
	stats.Emitted = response.Created + response.Refreshed
	stats.Removed = response.Removed
	stats.Unchanged = response.Unchanged
	stats.ReviewedConflicts = response.ReviewedConflicts
	if response.ReviewedConflicts > 0 {
		s.logger().Warn("resolve-sync: human-reviewed edges need manual resolution",
			"count", response.ReviewedConflicts, "scopeId", scopeID)
	}
	if s.OnSuccessfulSnapshot != nil {
		if saveErr := s.OnSuccessfulSnapshot(scopeID); saveErr != nil {
			s.logger().Warn("resolve-sync: graph committed but runtime state save failed",
				"scopeId", scopeID, "err", saveErr)
		}
	}
	return stats, nil
}
