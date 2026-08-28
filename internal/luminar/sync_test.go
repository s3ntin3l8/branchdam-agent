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

// The fixture (createFixtureCatalog, catalog_test.go) has 3 real pairs
// (image ids 101->102, 103->104, 105->106), 1 ambiguous candidate (109), 1
// no-source candidate (110), and 1 path-unresolvable pair (111->112). Every
// sync_test.go test below resolves at most the 101->102 pair through the
// node index, so the other candidates consistently land as "found but not
// emitted" rather than accidentally emitted.
const (
	sourcePath = "/Users/test/Pictures/IMG_1000.jpeg"
	editPath   = "/Users/test/Pictures/IMG_1000_upscale.jpg"
)

func TestSyncEmitsOnlyWhenBothEndpointsResolve(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	idx := fakeIndex{
		sourcePath: "source-uuid-1",
		editPath:   "target-uuid-1",
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
	if stats.PairsFound != 3 {
		t.Errorf("PairsFound = %d, want 3", stats.PairsFound)
	}
	if stats.Ambiguous != 1 {
		t.Errorf("Ambiguous = %d, want 1", stats.Ambiguous)
	}
	if stats.NoSourceInCatalog != 1 {
		t.Errorf("NoSourceInCatalog = %d, want 1", stats.NoSourceInCatalog)
	}
	if stats.PathUnresolvable != 1 {
		t.Errorf("PathUnresolvable = %d, want 1", stats.PathUnresolvable)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
	}
	// The panorama and trashed-source pairs are also PairsFound but have no
	// node-index entries, so they land as unresolved, not emitted.
	if stats.SourceUnresolved+stats.EditUnresolved != 2 {
		t.Errorf("SourceUnresolved+EditUnresolved = %d, want 2 (the other 2 real pairs, unresolved)",
			stats.SourceUnresolved+stats.EditUnresolved)
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
	if ev.SourceRowID != "101" || ev.EditRowID != "102" {
		t.Errorf("evidence row ids = (%q, %q), want (101, 102)", ev.SourceRowID, ev.EditRowID)
	}
	if ev.MatchRule != "filename-suffix" {
		t.Errorf("evidence.MatchRule = %q, want filename-suffix", ev.MatchRule)
	}
	if ev.Suffix != "_upscale" {
		t.Errorf("evidence.Suffix = %q, want _upscale", ev.Suffix)
	}
	if !ev.CameraModelMatch {
		t.Error("evidence.CameraModelMatch = false, want true")
	}
	if !ev.CaptureTimeMatch {
		t.Error("evidence.CaptureTimeMatch = false, want true")
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
		// source path missing; only the edit output is known. The fixture's
		// other 2 real pairs (panorama, trashed-source) have neither
		// endpoint in the index either, so all 3 land as source-unresolved.
		editPath: "target-uuid-1",
	}
	attacher := &fakeAttacher{}
	syncer := &Syncer{Catalog: cat, Index: idx, Client: attacher, AgentID: "a", CatalogPath: path}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.SourceUnresolved != 3 {
		t.Errorf("SourceUnresolved = %d, want 3", stats.SourceUnresolved)
	}
	// The real invariant under test -- an unresolved source never emits --
	// doesn't move if the fixture grows a new unrelated pair; SourceUnresolved
	// above does, so both are asserted.
	if stats.Emitted != 0 {
		t.Errorf("Emitted = %d, want 0 when the source master is unresolved", stats.Emitted)
	}
	if len(attacher.calls) != 0 {
		t.Errorf("expected no edges emitted when the 101->102 source master is unresolved, got %d calls", len(attacher.calls))
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
		sourcePath: "source-uuid-1",
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
	if stats.Emitted != 0 {
		t.Errorf("Emitted = %d, want 0 when the edit output is unresolved", stats.Emitted)
	}
	if len(attacher.calls) != 0 {
		t.Errorf("expected no edges emitted when the 101->102 edit output is unresolved, got %d calls", len(attacher.calls))
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
		sourcePath: "source-uuid-1",
		editPath:   "target-uuid-1",
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
		sourcePath: "source-uuid-1",
		editPath:   "target-uuid-1",
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
	// with PairsFound=0, proving Query actually replaces DefaultCatalogQuery
	// rather than being ignored.
	override := `SELECT '', '', '', '', 0, '', 0 WHERE 0`
	syncer := &Syncer{Catalog: cat, Index: fakeIndex{}, Client: &fakeAttacher{}, AgentID: "a", CatalogPath: path, Query: override}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.PairsFound != 0 {
		t.Errorf("PairsFound = %d, want 0 with the override query", stats.PairsFound)
	}
}

// TestSyncUsesDerivativeSuffixesOverride proves DerivativeSuffixes actually
// replaces DefaultDerivativeSuffixes: an override query exposing a single
// _hdr-suffixed pair the default suffix list would never match.
func TestSyncUsesDerivativeSuffixesOverride(t *testing.T) {
	dir := t.TempDir()
	path := createFixtureCatalog(t, dir)
	ctx := context.Background()

	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	override := `
SELECT '1', '/mnt', 'p', 'IMG_0001.jpeg', 0, '', 0
UNION ALL
SELECT '2', '/mnt', 'p', 'IMG_0001_hdr.jpg', 0, '', 0
`
	idx := fakeIndex{
		"/mnt/p/IMG_0001.jpeg":    "source-uuid",
		"/mnt/p/IMG_0001_hdr.jpg": "target-uuid",
	}
	attacher := &fakeAttacher{}
	syncer := &Syncer{
		Catalog:            cat,
		Index:              idx,
		Client:             attacher,
		AgentID:            "a",
		CatalogPath:        path,
		Query:              override,
		DerivativeSuffixes: []string{"_hdr"},
	}

	stats, err := syncer.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.PairsFound != 1 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v, want PairsFound=1 Emitted=1", stats)
	}
	if len(attacher.calls) != 1 {
		t.Fatalf("expected exactly 1 PostEdgeAttached call, got %d", len(attacher.calls))
	}
}
