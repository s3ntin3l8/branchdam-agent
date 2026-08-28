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
// "neo-db155" names the exact catalog version DefaultCatalogQuery and
// DefaultDerivativeSuffixes were verified against (see
// docs/luminar-catalog.md) -- not "verified" unqualified, since lineage
// itself is inferred from filenames, not read from the catalog, and only
// one Luminar Neo version has been checked. Bump this whenever
// DefaultCatalogQuery or DefaultDerivativeSuffixes changes in a way that
// could change which pairs get emitted -- it is what lets a future
// data-correction migration (the same pattern as branchDAM's own #132/00006)
// find every edge a particular schema-mapping version produced.
const SchemaMappingVersion = "neo-db155"

// Tier and Confidence are the plan's deliberately-conservative,
// already-decided values for a Luminar-sourced edge -- see issue #6 and the
// plan's M4 section. Confidence is intentionally below
// graph.AutoAcceptThresholdForTier(2) (0.90) so every edge lands in
// branchDAM's audit queue; do not change these without a corresponding
// server-side data-correction migration for edges already written at the
// lower tier, per the same reasoning as branchDAM's #132/00006. A
// zero-false-positive measurement across a real 995-image catalog (see
// docs/luminar-catalog.md) is what justifies holding 0.89 rather than
// lowering it further now that pairing is filename-inferred.
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

// Stats summarizes one Sync run. PairsFound is defined as "derivative
// candidates that resolved to exactly one source image in the catalog" --
// i.e. PairsFound == Emitted + SourceUnresolved + EditUnresolved always
// holds. Ambiguous and NoSourceInCatalog count candidates that never became
// a pair at all (so never reach node-index resolution), kept separate from
// PairsFound so "0 pairs found" (nothing looked derivative) is
// distinguishable from "candidates existed but weren't safe to pair."
type Stats struct {
	PairsFound        int // candidates with exactly one catalog source match
	Ambiguous         int // candidates with >1 stem match -- not paired
	NoSourceInCatalog int // candidates with 0 stem matches -- not paired
	PathUnresolvable  int // candidate or its source had no volume mount -- not paired
	SourceUnresolved  int // pairs skipped because SourcePath had no index entry
	EditUnresolved    int // pairs skipped because EditPath had no index entry
	Emitted           int // edges actually posted (or, in a dry run, that would have been)
	Errors            int // PostEdgeAttached calls that returned an error
}

// evidence is the evidenceJson object stamped onto every emitted edge.
// sourceRowId/editRowId let a human (or a future migration) trace an edge
// back to the exact catalog row it came from. matchRule/suffix/*Match record
// that the pair is filename-inferred, not read from relational lineage the
// catalog doesn't have -- see query.go's doc comment.
type evidence struct {
	SchemaMapping    string `json:"schemaMapping"`
	CatalogPath      string `json:"catalogPath"`
	SourcePath       string `json:"sourcePath"`
	EditPath         string `json:"editPath"`
	SourceRowID      string `json:"sourceRowId"`
	EditRowID        string `json:"editRowId"`
	MatchRule        string `json:"matchRule"`
	Suffix           string `json:"suffix"`
	CameraModelMatch bool   `json:"cameraModelMatch"`
	CaptureTimeMatch bool   `json:"captureTimeMatch"`
	SourceTrashed    bool   `json:"sourceTrashed"`
	EditTrashed      bool   `json:"editTrashed"`
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
//
// Trashed images are NOT filtered out of pairing or emission -- one of the
// two pairs this rule was verified against has its edit side trashed in
// Luminar, and the underlying file may well still exist on disk. If it
// doesn't, nodeindex simply won't resolve it and the pair lands in
// EditUnresolved like any other missing file; SourceTrashed/EditTrashed ride
// along in evidenceJson so the decision is visible, not silent.
type Syncer struct {
	Catalog            *Catalog
	Index              nodeindex.Resolver
	Client             EdgeAttacher
	AgentID            string
	CatalogPath        string
	Query              string   // defaults to DefaultCatalogQuery if empty
	DerivativeSuffixes []string // defaults to DefaultDerivativeSuffixes if empty
	DryRun             bool
	Logger             *slog.Logger
}

func (s *Syncer) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Sync reads catalog images from s.Catalog, infers edit->source pairs from
// filename convention (PairDerivatives), and emits an EVENT_EDGE_ATTACHED
// for each pair whose source and edit paths both resolve via s.Index. Every
// skip is logged (never silent) -- a pair with an unresolved endpoint is an
// expected outcome, not a bug, but an operator needs to see the count to
// know whether "0 edges emitted" means "nothing to do" or "the node index is
// missing entries."
func (s *Syncer) Sync(ctx context.Context) (Stats, error) {
	query := s.Query
	if query == "" {
		query = DefaultCatalogQuery
	}
	suffixes := s.DerivativeSuffixes
	if len(suffixes) == 0 {
		suffixes = DefaultDerivativeSuffixes
	}

	images, err := s.Catalog.Images(ctx, query)
	if err != nil {
		return Stats{}, fmt.Errorf("luminar: read catalog images: %w", err)
	}

	pairs, amb := PairDerivatives(images, suffixes)

	var stats Stats
	stats.PairsFound = len(pairs)
	for _, a := range amb {
		switch a.Reason {
		case ReasonAmbiguous:
			stats.Ambiguous++
			s.logger().Info("luminar-sync: skipping candidate, ambiguous stem match",
				"fileName", a.FileName, "suffix", a.Suffix, "matches", a.Matches)
		case ReasonPathUnresolvable:
			stats.PathUnresolvable++
			s.logger().Info("luminar-sync: skipping candidate, no volume mount to build a path",
				"fileName", a.FileName, "suffix", a.Suffix)
		default:
			stats.NoSourceInCatalog++
			s.logger().Info("luminar-sync: skipping candidate, no source image in catalog",
				"fileName", a.FileName, "suffix", a.Suffix)
		}
	}

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
			SchemaMapping:    SchemaMappingVersion,
			CatalogPath:      s.CatalogPath,
			SourcePath:       p.SourcePath,
			EditPath:         p.EditPath,
			SourceRowID:      p.SourceRowID,
			EditRowID:        p.EditRowID,
			MatchRule:        "filename-suffix",
			Suffix:           p.Suffix,
			CameraModelMatch: p.CameraModelMatch,
			CaptureTimeMatch: p.CaptureTimeMatch,
			SourceTrashed:    p.SourceTrashed,
			EditTrashed:      p.EditTrashed,
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
