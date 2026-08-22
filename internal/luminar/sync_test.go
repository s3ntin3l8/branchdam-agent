package luminar

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// fakeIndex is a nodeindex.Resolver over a plain map, for tests that don't
// need nodeindex.FileIndex's JSON-loading behavior.
type fakeIndex map[string]string

func (f fakeIndex) Resolve(path string) (string, bool, error) {
	uuid, ok := f[path]
	return uuid, ok, nil
}

// erroringIndex always returns an error, to exercise Sync's error path.
type erroringIndex struct{}

func (erroringIndex) Resolve(string) (string, bool, error) {
	return "", false, errors.New("boom")
}

// fakeAttacher records every EdgeAttachedPayload it's given, or returns a
// canned error if failOn matches the target's uuid.
type fakeAttacher struct {
	calls  []branchdam.EdgeAttachedPayload
	failOn string
}

func (f *fakeAttacher) PostEdgeAttached(_ context.Context, agentID string, payload branchdam.EdgeAttachedPayload) (*branchdam.EventResponse, error) {
	if payload.TargetNodeUUID == f.failOn {
		return nil, errors.New("simulated server error")
	}
	f.calls = append(f.calls, payload)
	return &branchdam.EventResponse{EventID: "evt-" + agentID}, nil
}

func TestSyncEmitsOnlyWhenBothEndpointsResolve(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	// Only the fixture's one exported-edit pair
	// (/masters/DSC_0001.NEF -> /exports/DSC_0001-edit.jpg) is a candidate;
	// resolve both sides.
	idx := fakeIndex{
		"/masters/DSC_0001.NEF":      "source-uuid-1",
		"/exports/DSC_0001-edit.jpg": "target-uuid-1",
	}
	attacher := &fakeAttacher{}

	syncer := &Syncer{
		Catalog:     cat,
		Index:       idx,
		Client:      attacher,
		AgentID:     "test-agent",
		CatalogPath: path,
	}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.PairsFound != 1 {
		t.Errorf("PairsFound = %d, want 1", stats.PairsFound)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
	}
	if stats.SourceUnresolved != 0 || stats.EditUnresolved != 0 {
		t.Errorf("expected no unresolved pairs, got source=%d edit=%d", stats.SourceUnresolved, stats.EditUnresolved)
	}

	if len(attacher.calls) != 1 {
		t.Fatalf("expected exactly 1 PostEdgeAttached call, got %d", len(attacher.calls))
	}
	got := attacher.calls[0]

	// Direction: source = master (parent), target = edit (child) -- pinned
	// by named field, not position, per branchDAM's DERIVED_FROM convention.
	if got.SourceNodeUUID != "source-uuid-1" {
		t.Errorf("SourceNodeUUID = %q, want source-uuid-1 (the master)", got.SourceNodeUUID)
	}
	if got.TargetNodeUUID != "target-uuid-1" {
		t.Errorf("TargetNodeUUID = %q, want target-uuid-1 (the edit/export)", got.TargetNodeUUID)
	}
	if got.RelationshipType != branchdam.RelationshipDerivedFrom {
		t.Errorf("RelationshipType = %q, want %q", got.RelationshipType, branchdam.RelationshipDerivedFrom)
	}
	if got.Tier != Tier {
		t.Errorf("Tier = %d, want %d", got.Tier, Tier)
	}
	if got.Confidence != Confidence {
		t.Errorf("Confidence = %v, want %v", got.Confidence, Confidence)
	}
	if got.Confidence >= 0.90 {
		t.Errorf("Confidence = %v must stay below branchDAM's tier-2 auto-accept threshold (0.90) -- every edge must land in the audit queue", got.Confidence)
	}
	if got.Resolver != ResolverName {
		t.Errorf("Resolver = %q, want %q", got.Resolver, ResolverName)
	}

	var ev evidence
	if err := json.Unmarshal(got.EvidenceJSON, &ev); err != nil {
		t.Fatalf("unmarshal evidenceJson: %v", err)
	}
	if ev.SchemaMapping != SchemaMappingVersion {
		t.Errorf("evidence.SchemaMapping = %q, want %q", ev.SchemaMapping, SchemaMappingVersion)
	}
	if ev.SourceRowID != "1" || ev.EditRowID != "1" {
		t.Errorf("evidence row ids = (%q, %q), want (1, 1)", ev.SourceRowID, ev.EditRowID)
	}
}

func TestSyncSkipsWhenSourceUnresolved(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	idx := fakeIndex{
		// source path missing; only the edit output is known
		"/exports/DSC_0001-edit.jpg": "target-uuid-1",
	}
	attacher := &fakeAttacher{}
	syncer := &Syncer{Catalog: cat, Index: idx, Client: attacher, AgentID: "a", CatalogPath: path}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.SourceUnresolved != 1 {
		t.Errorf("SourceUnresolved = %d, want 1", stats.SourceUnresolved)
	}
	if stats.Emitted != 0 || len(attacher.calls) != 0 {
		t.Errorf("expected no edges emitted when the source master is unresolved, got %d calls", len(attacher.calls))
	}
}

func TestSyncSkipsWhenEditUnresolved(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	idx := fakeIndex{
		// edit path missing; only the source master is known -- this is the
		// realistic "master ingested, Luminar export was scanned normally
		// and its nodeUuid isn't known to the agent" case.
		"/masters/DSC_0001.NEF": "source-uuid-1",
	}
	attacher := &fakeAttacher{}
	syncer := &Syncer{Catalog: cat, Index: idx, Client: attacher, AgentID: "a", CatalogPath: path}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.EditUnresolved != 1 {
		t.Errorf("EditUnresolved = %d, want 1", stats.EditUnresolved)
	}
	if stats.Emitted != 0 || len(attacher.calls) != 0 {
		t.Errorf("expected no edges emitted when the edit output is unresolved, got %d calls", len(attacher.calls))
	}
}

func TestSyncDryRunNeverCallsClient(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	idx := fakeIndex{
		"/masters/DSC_0001.NEF":      "source-uuid-1",
		"/exports/DSC_0001-edit.jpg": "target-uuid-1",
	}
	attacher := &fakeAttacher{}
	syncer := &Syncer{Catalog: cat, Index: idx, Client: attacher, AgentID: "a", CatalogPath: path, DryRun: true}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1 (dry run still counts what it would emit)", stats.Emitted)
	}
	if len(attacher.calls) != 0 {
		t.Errorf("dry run must never call the client, got %d calls", len(attacher.calls))
	}
}

func TestSyncCountsClientErrors(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	idx := fakeIndex{
		"/masters/DSC_0001.NEF":      "source-uuid-1",
		"/exports/DSC_0001-edit.jpg": "target-uuid-1",
	}
	attacher := &fakeAttacher{failOn: "target-uuid-1"}
	syncer := &Syncer{Catalog: cat, Index: idx, Client: attacher, AgentID: "a", CatalogPath: path}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1", stats.Errors)
	}
	if stats.Emitted != 0 {
		t.Errorf("Emitted = %d, want 0 when the client call failed", stats.Emitted)
	}
}

func TestSyncPropagatesIndexResolveError(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	syncer := &Syncer{Catalog: cat, Index: erroringIndex{}, Client: &fakeAttacher{}, AgentID: "a", CatalogPath: path}

	if _, err := syncer.Sync(ctx); err == nil {
		t.Fatal("expected Sync to propagate a node-index Resolve error, got nil")
	}
}

func TestSyncUsesQueryOverride(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	// An override query that returns zero rows should short-circuit Sync
	// with PairsFound=0, proving Query actually replaces
	// DefaultEditSourceQuery rather than being ignored.
	override := `SELECT '' , '', '', '' WHERE 0`
	syncer := &Syncer{Catalog: cat, Index: fakeIndex{}, Client: &fakeAttacher{}, AgentID: "a", CatalogPath: path, Query: override}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.PairsFound != 0 {
		t.Errorf("PairsFound = %d, want 0 with the override query", stats.PairsFound)
	}
}
