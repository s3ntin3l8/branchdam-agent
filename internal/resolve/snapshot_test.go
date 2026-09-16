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

func TestScopeIDIgnoresCredentialsAndQuery(t *testing.T) {
	a := ScopeID("postgres://alice:secret@db.local/Projects?sslmode=disable")
	b := ScopeID("postgres://bob:new@db.local/Projects?sslmode=require")
	if a != b || len(a) != 64 {
		t.Fatalf("scope identity changed with credentials/query: %q %q", a, b)
	}
}
