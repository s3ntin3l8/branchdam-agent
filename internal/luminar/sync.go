package luminar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/nodeindex"
)

// SchemaMappingVersion is stamped into every emitted edge's evidenceJson.
// Bump it whenever DefaultEditSourceQuery (or an operator's --query-file
// override, if it becomes the new default) changes in a way that could
// change which rows a query returns -- it is what lets a future
// data-correction migration (the same pattern as branchDAM's own #132/00006)
// find every edge a particular, possibly-wrong schema guess produced, the
// same way the plan's M4 section describes for the eventual tier-1
// promotion.
const SchemaMappingVersion = "v1-unverified"

// Tier and Confidence are the plan's deliberately-conservative,
// already-decided values for a Luminar-sourced edge -- see issue #6 and the
// plan's M4 section. Confidence is intentionally below
// graph.AutoAcceptThresholdForTier(2) (0.90) so every edge lands in
// branchDAM's audit queue; do not change these without a corresponding
// server-side data-correction migration for edges already written at the
// lower tier, per the same reasoning as branchDAM's #132/00006.
const (
	Tier             = 2
	Confidence       = 0.89
	RelationshipType = branchdam.RelationshipDerivedFrom
	ResolverName     = "luminar_catalog"
)

// EdgeAttacher is the subset of *branchdam.Client's surface Syncer needs, so
// tests can substitute a fake without a real HTTP server.
type EdgeAttacher interface {
	PostEdgeAttached(ctx context.Context, agentID string, payload branchdam.EdgeAttachedPayload) (*branchdam.EventResponse, error)
}

// Stats summarizes one Sync run.
type Stats struct {
	PairsFound       int // rows EditSourcePairs returned
	SourceUnresolved int // pairs skipped because SourcePath had no index entry
	EditUnresolved   int // pairs skipped because EditPath had no index entry
	Emitted          int // edges actually posted (or, in a dry run, that would have been)
	Errors           int // PostEdgeAttached calls that returned an error
}

// evidence is the evidenceJson object stamped onto every emitted edge.
// sourceRowId/editRowId let a human (or a future migration) trace an edge
// back to the exact catalog row it came from, which matters precisely
// because DefaultEditSourceQuery's column mapping is unverified -- see its
// doc comment.
type evidence struct {
	SchemaMapping string `json:"schemaMapping"`
	CatalogPath   string `json:"catalogPath"`
	SourcePath    string `json:"sourcePath"`
	EditPath      string `json:"editPath"`
	SourceRowID   string `json:"sourceRowId"`
	EditRowID     string `json:"editRowId"`
}

// Syncer resolves EditSourcePair.SourcePath/EditPath to nodeUuids via a
// nodeindex.Resolver and emits EVENT_EDGE_ATTACHED for every pair where BOTH
// sides resolve. Per issue #6's node-resolution scope decision (see
// docs/luminar-catalog.md): this is narrower than "agent-ingested masters
// only" -- branchDAM's applyEdgeAttached (internal/agent/drainer.go) resolves
// BOTH sourceNodeUuid and targetNodeUuid via GetMediaNodeByUUID, and an
// unresolvable target is not a fatal error server-side (it's a wrapped
// sql.ErrNoRows that matches neither the fatal nor transient substring
// classification in internal/branchdam/errors.go), so it would silently burn
// the full retry budget and land FAILED with no feedback channel. A pair is
// only emitted when the index has an entry for both the source master AND
// the edit/export output -- i.e. both files must have been agent-ingested
// (or otherwise recorded in the index) before luminar-sync runs.
type Syncer struct {
	Catalog     *Catalog
	Index       nodeindex.Resolver
	Client      EdgeAttacher
	AgentID     string
	CatalogPath string
	Query       string // defaults to DefaultEditSourceQuery if empty
	DryRun      bool
	Logger      *slog.Logger
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Sync reads edit->source pairs from s.Catalog and emits a
// EVENT_EDGE_ATTACHED for each pair whose source and edit paths both resolve
// via s.Index. Every skip is logged (never silent) -- a pair with an
// unresolved endpoint is an expected outcome, not a bug, but an operator
// needs to see the count to know whether "0 edges emitted" means "nothing to
// do" or "the node index is missing entries."
func (s *Syncer) Sync(ctx context.Context) (Stats, error) {
	query := s.Query
	if query == "" {
		query = DefaultEditSourceQuery
	}

	pairs, err := s.Catalog.EditSourcePairs(ctx, query)
	if err != nil {
		return Stats{}, fmt.Errorf("luminar: read edit-source pairs: %w", err)
	}

	var stats Stats
	stats.PairsFound = len(pairs)

	for _, p := range pairs {
		sourceUUID, sourceOK, err := s.Index.Resolve(p.SourcePath)
		if err != nil {
			return stats, fmt.Errorf("luminar: resolve source path %q: %w", p.SourcePath, err)
		}
		editUUID, editOK, err := s.Index.Resolve(p.EditPath)
		if err != nil {
			return stats, fmt.Errorf("luminar: resolve edit path %q: %w", p.EditPath, err)
		}

		if !sourceOK {
			stats.SourceUnresolved++
			s.logger().Info("luminar-sync: skipping pair, source not in node index",
				"sourcePath", p.SourcePath, "editPath", p.EditPath)
			continue
		}
		if !editOK {
			stats.EditUnresolved++
			s.logger().Info("luminar-sync: skipping pair, edit output not in node index",
				"sourcePath", p.SourcePath, "editPath", p.EditPath)
			continue
		}

		ev := evidence{
			SchemaMapping: SchemaMappingVersion,
			CatalogPath:   s.CatalogPath,
			SourcePath:    p.SourcePath,
			EditPath:      p.EditPath,
			SourceRowID:   p.SourceRowID,
			EditRowID:     p.EditRowID,
		}
		evJSON, err := json.Marshal(ev)
		if err != nil {
			return stats, fmt.Errorf("luminar: marshal evidence for %q -> %q: %w", p.SourcePath, p.EditPath, err)
		}

		payload := branchdam.EdgeAttachedPayload{
			// Source = master/original = the graph parent; Target =
			// edit/export = the graph child -- matches branchDAM's
			// DERIVED_FROM convention (internal/graph/resolvers.go's
			// inferRelationship: parent=source, child=target) used by
			// every other resolver.
			SourceNodeUUID:   sourceUUID,
			TargetNodeUUID:   editUUID,
			RelationshipType: RelationshipType,
			Confidence:       Confidence,
			Tier:             Tier,
			Resolver:         ResolverName,
			EvidenceJSON:     evJSON,
		}

		if s.DryRun {
			s.logger().Info("luminar-sync: (dry run) would emit edge",
				"sourceNodeUuid", sourceUUID, "targetNodeUuid", editUUID,
				"sourcePath", p.SourcePath, "editPath", p.EditPath)
			stats.Emitted++
			continue
		}

		if _, err := s.Client.PostEdgeAttached(ctx, s.AgentID, payload); err != nil {
			stats.Errors++
			s.logger().Error("luminar-sync: PostEdgeAttached failed",
				"sourceNodeUuid", sourceUUID, "targetNodeUuid", editUUID, "err", err)
			continue
		}
		stats.Emitted++
		s.logger().Info("luminar-sync: emitted edge",
			"sourceNodeUuid", sourceUUID, "targetNodeUuid", editUUID,
			"sourcePath", p.SourcePath, "editPath", p.EditPath)
	}

	return stats, nil
}
