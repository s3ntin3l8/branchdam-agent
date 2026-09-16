package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

type fakeSnapshotSubmitter struct {
	calls    []branchdam.ResolveSnapshot
	response branchdam.ResolveSnapshotResponse
	err      error
}

func (f *fakeSnapshotSubmitter) PostResolveSnapshot(_ context.Context, snapshot branchdam.ResolveSnapshot) (*branchdam.ResolveSnapshotResponse, error) {
	f.calls = append(f.calls, snapshot)
	if f.err != nil {
		return nil, f.err
	}
	return &f.response, nil
}

func TestSnapshotAggregatesPlacementsAndProtectsUnresolved(t *testing.T) {
	clipRows := []struct {
		timelineID, timelineName, seqID, trackID, itemID, name, mediaPath, inPoint, start, duration string
	}{
		{"tl1", "Master", "seq1", "track1", "item-b", "clip B", `D:\Videos\a.mov`, "2", "20", "30"},
		{"tl1", "Master", "seq1", "track1", "item-a", "clip A", `D:\Videos\a.mov`, "1", "10", "30"},
		{"tl1", "Master", "seq1", "track1", "item-c", "missing", `D:\Videos\missing.mov`, "", "40", "10"},
	}
	db := openTestDBForSyncWithClips(t, clipRows)
	defer func() { _ = db.Close() }()
	client := &fakeSnapshotSubmitter{response: branchdam.ResolveSnapshotResponse{Created: 1, Unresolved: 1}}
	saved := false
	expectedScope := ScopeID("file:/tmp/resolve.db?mode=ro")
	s := &Syncer{
		DB: db, AgentID: "agent-a", DatabaseURL: "file:/tmp/resolve.db?mode=ro",
		UseSnapshot: true, SnapshotClient: client,
		Index:                &fakeIndex{entries: map[string]string{"/storage/a.mov": "018f0000-0000-7000-8000-000000000101"}},
		PathRewrites:         []PathRewrite{{From: `D:\Videos\`, To: "/storage/"}},
		LegacyTimelineIDs:    []string{"old-tl"},
		OnSuccessfulSnapshot: func(scope string) error { saved = scope == expectedScope; return nil },
	}
	stats, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || !saved || stats.Emitted != 1 || stats.Unresolved != 1 {
		t.Fatalf("snapshot result: calls=%d saved=%v stats=%+v", len(client.calls), saved, stats)
	}
	request := client.calls[0]
	if len(request.Timelines) != 1 || len(request.Memberships) != 2 || len(request.LegacyTimelineNodeUUIDs) != 1 {
		t.Fatalf("snapshot shape = %+v", request)
	}
	var ev struct {
		Placements []snapshotPlacement `json:"placements"`
	}
	if err := json.Unmarshal(request.Memberships[0].EvidenceJSON, &ev); err != nil {
		t.Fatal(err)
	}
	if len(ev.Placements) != 2 || ev.Placements[0].ItemID != "item-a" || ev.Placements[1].ItemID != "item-b" {
		t.Fatalf("placements = %+v", ev.Placements)
	}
	if request.Memberships[1].SourceNodeUUID != "" {
		t.Fatal("unresolved member should be present without source UUID")
	}
}

func TestSnapshotFailureDoesNotAdvanceState(t *testing.T) {
	db := openTestDBForSyncWithClips(t, []struct {
		timelineID, timelineName, seqID, trackID, itemID, name, mediaPath, inPoint, start, duration string
	}{{"tl1", "Master", "seq1", "track1", "item1", "a", `D:\Videos\a.mov`, "", "", ""}})
	defer func() { _ = db.Close() }()
	client := &fakeSnapshotSubmitter{err: errors.New("server returned HTTP 404")}
	saves := 0
	s := &Syncer{DB: db, AgentID: "agent-a", DatabaseURL: "file:/tmp/resolve.db",
		UseSnapshot: true, SnapshotClient: client,
		Index:                &fakeIndex{entries: map[string]string{"/storage/a.mov": "018f0000-0000-7000-8000-000000000101"}},
		PathRewrites:         []PathRewrite{{From: `D:\Videos\`, To: "/storage/"}},
		OnSuccessfulSnapshot: func(string) error { saves++; return nil },
	}
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("failed snapshot error = %v", err)
	}
	if saves != 0 {
		t.Fatal("failed server reconciliation advanced runtime scope")
	}
}

func TestSnapshotStatsOriginalPathNotContainerPath(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "a.mov")
	if err := os.WriteFile(original, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := openTestDBForSyncWithClips(t, []struct {
		timelineID, timelineName, seqID, trackID, itemID, name, mediaPath, inPoint, start, duration string
	}{{"tl1", "Master", "seq1", "track1", "item1", "a", original, "", "", ""}})
	defer func() { _ = db.Close() }()
	client := &fakeSnapshotSubmitter{}
	s := &Syncer{DB: db, AgentID: "agent-a", DatabaseURL: "file:/tmp/resolve.db",
		UseSnapshot: true, SnapshotClient: client,
		PathRewrites: []PathRewrite{{From: dir + string(os.PathSeparator), To: "/server-only/"}},
		Index:        &fakeIndex{entries: map[string]string{"/server-only/a.mov": "018f0000-0000-7000-8000-000000000101"}},
	}
	stats, err := s.Sync(context.Background())
	if err != nil || stats.FileMissing != 0 {
		t.Fatalf("server-only path wrongly reported missing: stats=%+v err=%v", stats, err)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Sync(context.Background())
	if err != nil || stats.FileMissing != 1 {
		t.Fatalf("missing native path not detected: stats=%+v err=%v", stats, err)
	}
}

func TestSnapshotMergesPathAliasesResolvedToSameNode(t *testing.T) {
	clipRows := []struct {
		timelineID, timelineName, seqID, trackID, itemID, name, mediaPath, inPoint, start, duration string
	}{
		{"tl1", "Master", "seq1", "track1", "item-a", "upper", `D:\Videos\A.mov`, "", "10", "20"},
		{"tl1", "Master", "seq1", "track1", "item-b", "lower", `D:\Videos\a.mov`, "", "30", "20"},
	}
	db := openTestDBForSyncWithClips(t, clipRows)
	defer func() { _ = db.Close() }()
	client := &fakeSnapshotSubmitter{}
	s := &Syncer{
		DB: db, AgentID: "agent-a", DatabaseURL: "file:/tmp/resolve.db", UseSnapshot: true, SnapshotClient: client,
		Index: &fakeIndex{entries: map[string]string{
			"/storage/A.mov": "018f0000-0000-7000-8000-000000000101",
			"/storage/a.mov": "018f0000-0000-7000-8000-000000000101",
		}},
		PathRewrites: []PathRewrite{{From: `D:\Videos\`, To: "/storage/"}},
	}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 1 || len(client.calls[0].Memberships) != 1 {
		t.Fatalf("alias memberships were not merged: %+v", client.calls)
	}
	member := client.calls[0].Memberships[0]
	var ev snapshotEvidence
	if err := json.Unmarshal(member.EvidenceJSON, &ev); err != nil {
		t.Fatal(err)
	}
	if member.MediaFilePath != `D:\Videos\A.mov` || len(ev.MediaFilePaths) != 2 || len(ev.Placements) != 2 {
		t.Fatalf("merged alias evidence = member=%+v evidence=%+v", member, ev)
	}
}

func TestScopeIDIgnoresCredentialsAndTransportQuery(t *testing.T) {
	a := ScopeID("postgres://alice:secret@db.local/Projects?sslmode=disable")
	b := ScopeID("postgres://bob:new@db.local/Projects?sslmode=require")
	if a != b || len(a) != 64 {
		t.Fatalf("scope identity changed with credentials/transport query: %q %q", a, b)
	}
}

func TestScopeIDIncludesPostgresQueryDatabaseIdentity(t *testing.T) {
	cases := [][2]string{
		{"postgres://db.local/?dbname=ResolveA", "postgres://db.local/?dbname=ResolveB"},
		{"postgres://db.local/?service=resolve-a", "postgres://db.local/?service=resolve-b"},
		{"postgres:///?host=db-a&port=5432", "postgres:///?host=db-b&port=5433"},
		{"postgres://db.local/resolve?search_path=project_a", "postgres://db.local/resolve?search_path=project_b"},
	}
	for _, pair := range cases {
		if ScopeID(pair[0]) == ScopeID(pair[1]) {
			t.Fatalf("distinct PostgreSQL identities collided: %q and %q", pair[0], pair[1])
		}
	}
	a := ScopeID("postgres://db.local/?search_path=project&dbname=resolve")
	b := ScopeID("postgres://db.local/?dbname=resolve&search_path=project")
	if a != b {
		t.Fatal("query parameter ordering changed canonical database identity")
	}
}

func TestLegacyVirtualNodeUUIDUsesPreSnapshotDatabaseIdentity(t *testing.T) {
	const databaseURL = "postgres://alice:secret@db.local/?dbname=ResolveA&sslmode=require" // pragma: allowlist secret
	legacy := legacyVirtualNodeUUID("agent-a", "tl1", databaseURL)
	wantLegacy := VirtualNodeUUID("agent-a", "tl1", "postgres://db.local/")
	if legacy != wantLegacy {
		t.Fatalf("legacy UUID = %q, want pre-snapshot UUID %q", legacy, wantLegacy)
	}
	if legacy == VirtualNodeUUID("agent-a", "tl1", databaseURL) {
		t.Fatal("legacy and current UUID unexpectedly collide for a query-selected database")
	}
}
