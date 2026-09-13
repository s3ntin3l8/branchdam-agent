package resolve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

func TestRewritePath(t *testing.T) {
	rewrites := []PathRewrite{
		{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		{From: "F:\\", To: "/storage/archive/"},
	}

	cases := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{
			"matching prefix",
			`D:\Videos\Norway 2025\Pixel\PXL_001.mp4`,
			"/storage/archive/videos/Norway 2025/Pixel/PXL_001.mp4",
			true,
		},
		{
			"longest prefix wins",
			`D:\Videos\Norway 2025\PXL_001.mp4`,
			"/storage/archive/videos/Norway 2025/PXL_001.mp4",
			true,
		},
		{
			"second prefix",
			`F:\Timelapse\001_0193\TIMELAPSE_[0001-2214].DNG`,
			"/storage/archive/Timelapse/001_0193/TIMELAPSE_[0001-2214].DNG",
			true,
		},
		{
			"no match",
			`/mnt/card/DCIM/IMG_001.JPG`,
			"",
			false,
		},
		{
			"empty path",
			"",
			"",
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rewritePath(rewrites, tc.input)
			if ok != tc.wantOK {
				t.Errorf("rewritePath(%q) ok = %v, want %v", tc.input, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("rewritePath(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRewritePathNoRewrites(t *testing.T) {
	got, ok := rewritePath(nil, `D:\Videos\test.mp4`)
	if ok {
		t.Error("expected ok=false with no rewrites")
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

func TestSchemaMappingVersion(t *testing.T) {
	if SchemaMappingVersion == "" {
		t.Error("SchemaMappingVersion must not be empty")
	}
}

func TestOpenSQLite(t *testing.T) {
	// Open a :memory: SQLite database to verify the driver works.
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
}

func TestOpenUnsupportedScheme(t *testing.T) {
	_, err := Open(context.Background(), "mysql://user:pass@localhost/db")
	if err == nil {
		t.Error("expected error for unsupported scheme")
	}
}

func TestTimelineClipsEmpty(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Create the full schema with no rows.
	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	clips, err := db.TimelineClips(context.Background(), DefaultTimelineQuery)
	if err != nil {
		t.Fatalf("TimelineClips: %v", err)
	}
	if len(clips) != 0 {
		t.Errorf("expected 0 clips, got %d", len(clips))
	}
}

func TestTimelineClipsWithData(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Create a minimal schema matching the query's join chain.
	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert test data.
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\Norway\PXL_001.mp4', '262', '96319', '33', 'tr1');
		INSERT INTO "Sm2TiItem" VALUES ('i2', 'DJI_001.mp4', 'D:\Videos\Norway\DJI_001.mp4', NULL, '0', '120', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	clips, err := db.TimelineClips(context.Background(), DefaultTimelineQuery)
	if err != nil {
		t.Fatalf("TimelineClips: %v", err)
	}
	if len(clips) != 2 {
		t.Fatalf("expected 2 clips, got %d", len(clips))
	}

	// Verify first clip.
	if clips[0].TimelineName != "Master" {
		t.Errorf("TimelineName = %q, want %q", clips[0].TimelineName, "Master")
	}
	if clips[0].MediaFilePath != `D:\Videos\Norway\PXL_001.mp4` {
		t.Errorf("MediaFilePath = %q, want %q", clips[0].MediaFilePath, `D:\Videos\Norway\PXL_001.mp4`)
	}
	if !clips[0].InPoint.Valid || clips[0].InPoint.String != "262" {
		t.Errorf("InPoint = %v, want valid '262'", clips[0].InPoint)
	}

	// Verify second clip has NULL in_point.
	if clips[1].InPoint.Valid {
		t.Errorf("InPoint should be NULL for second clip, got %q", clips[1].InPoint.String)
	}
}

func TestSyncerDryRun(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\Norway\PXL_001.mp4', '262', '96319', '33', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/videos/Norway/PXL_001.mp4": "node-uuid-001",
	}}

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "file::memory:",
		DryRun:      true,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.ClipsFound != 1 {
		t.Errorf("ClipsFound = %d, want 1", stats.ClipsFound)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1 (dry run counts would-be emissions)", stats.Emitted)
	}
	if stats.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", stats.Unresolved)
	}
	if stats.NoRewrite != 0 {
		t.Errorf("NoRewrite = %d, want 0", stats.NoRewrite)
	}
}

func TestSyncerLiveEmitsToServer(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\Norway\PXL_001.mp4', '262', '96319', '33', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/videos/Norway/PXL_001.mp4": "node-uuid-001",
	}}

	var gotPayload branchdam.EdgeAttachedPayload
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		var env branchdam.EventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if err := json.Unmarshal([]byte(env.Payload), &gotPayload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-resolve-sync"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	syncer := &Syncer{
		DB:          db,
		Index:       index,
		Client:      client,
		AgentID:     "test-agent",
		DatabaseURL: "file::memory:",
		DryRun:      false,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
	}
	if gotPayload.SourceNodeUUID != "node-uuid-001" {
		t.Errorf("SourceNodeUUID = %q, want %q", gotPayload.SourceNodeUUID, "node-uuid-001")
	}
	if gotPayload.TargetNodeUUID != "node-uuid-001" {
		t.Errorf("TargetNodeUUID = %q, want %q (self-edge for metadata)", gotPayload.TargetNodeUUID, "node-uuid-001")
	}
	if gotPayload.RelationshipType != branchdam.RelationshipProjectSidecar {
		t.Errorf("RelationshipType = %q, want %q", gotPayload.RelationshipType, branchdam.RelationshipProjectSidecar)
	}
	if gotPayload.Confidence != 1.00 {
		t.Errorf("Confidence = %f, want 1.00", gotPayload.Confidence)
	}
	if gotPayload.Resolver != ResolverName {
		t.Errorf("Resolver = %q, want %q", gotPayload.Resolver, ResolverName)
	}

	// Verify evidence JSON.
	var ev evidence
	if err := json.Unmarshal(gotPayload.EvidenceJSON, &ev); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	if ev.SchemaMapping != SchemaMappingVersion {
		t.Errorf("evidence.schemaMapping = %q, want %q", ev.SchemaMapping, SchemaMappingVersion)
	}
	if ev.TimelineName != "Master" {
		t.Errorf("evidence.timelineName = %q, want %q", ev.TimelineName, "Master")
	}
	if ev.ClipName != "PXL_001.mp4" {
		t.Errorf("evidence.clipName = %q, want %q", ev.ClipName, "PXL_001.mp4")
	}
	if ev.InPoint != "262" {
		t.Errorf("evidence.inPoint = %q, want %q", ev.InPoint, "262")
	}
}

func TestSyncerUnresolvedPath(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\Norway\PXL_001.mp4', '262', '96319', '33', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	// Empty index — no paths resolve.
	index := &fakeIndex{entries: map[string]string{}}

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "file::memory:",
		DryRun:      true,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", stats.Unresolved)
	}
	if stats.Emitted != 0 {
		t.Errorf("Emitted = %d, want 0", stats.Emitted)
	}
}

func TestSyncerNoRewriteMatch(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'E:\Other\PXL_001.mp4', '262', '96319', '33', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	index := &fakeIndex{entries: map[string]string{}}

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "file::memory:",
		DryRun:      true,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.NoRewrite != 1 {
		t.Errorf("NoRewrite = %d, want 1", stats.NoRewrite)
	}
}

func TestSyncerDeduplicatesByPathAndTimeline(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	schema := `
		CREATE TABLE "Sm2TiItem" (
			"Sm2TiItem_id" TEXT PRIMARY KEY,
			"Name" TEXT,
			"MediaFilePath" TEXT,
			"In" TEXT,
			"Start" TEXT,
			"Duration" TEXT,
			"Sm2TiTrack_id" TEXT
		);
		CREATE TABLE "Sm2TiTrack" (
			"Sm2TiTrack_id" TEXT PRIMARY KEY,
			"Type" INTEGER,
			"Sequence" TEXT
		);
		CREATE TABLE "Sm2Sequence" (
			"Sm2Sequence_id" TEXT PRIMARY KEY,
			"Sm2Timeline_id" TEXT
		);
		CREATE TABLE "Sm2Timeline" (
			"Sm2Timeline_id" TEXT PRIMARY KEY,
			"Name" TEXT
		);
	`
	if _, err := db.db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// Same file appears in the same timeline twice (different in/out points).
	data := `
		INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master');
		INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1');
		INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1');
		INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '100', '0', '30', 'tr1');
		INSERT INTO "Sm2TiItem" VALUES ('i2', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '200', '30', '30', 'tr1');
	`
	if _, err := db.db.ExecContext(context.Background(), data); err != nil {
		t.Fatalf("insert data: %v", err)
	}

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/PXL_001.mp4": "node-uuid-001",
	}}

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "file::memory:",
		DryRun:      true,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Same file + same timeline = deduplicated to 1.
	if stats.ClipsFound != 1 {
		t.Errorf("ClipsFound = %d, want 1 (deduped)", stats.ClipsFound)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
	}
}

// fakeIndex is a nodeindex.Resolver backed by an in-memory map.
type fakeIndex struct {
	entries map[string]string
}

func (f *fakeIndex) Resolve(path string) (string, bool, error) {
	uuid, ok := f.entries[path]
	return uuid, ok, nil
}
