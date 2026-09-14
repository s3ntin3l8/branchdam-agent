package resolve

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

const resolveSchema = `
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

func insertClip(t *testing.T, db *DB, timelineID, timelineName, seqID, trackID, itemID, name, mediaPath, inPoint, start, duration string) {
	t.Helper()
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES (?, ?)`, timelineID, timelineName); err != nil {
		t.Fatalf("insert timeline: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Sequence" VALUES (?, ?)`, seqID, timelineID); err != nil {
		t.Fatalf("insert sequence: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiTrack" VALUES (?, 0, ?)`, trackID, seqID); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES (?, ?, ?, ?, ?, ?, ?)`, itemID, name, mediaPath, inPoint, start, duration, trackID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
}

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

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
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

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert test data.
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master')`); err != nil {
		t.Fatalf("insert timeline: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1')`); err != nil {
		t.Fatalf("insert sequence: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1')`); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\Norway\PXL_001.mp4', '262', '96319', '33', 'tr1')`); err != nil {
		t.Fatalf("insert item 1: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i2', 'DJI_001.mp4', 'D:\Videos\Norway\DJI_001.mp4', NULL, '0', '120', 'tr1')`); err != nil {
		t.Fatalf("insert item 2: %v", err)
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

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "t1", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `D:\Videos\Norway\PXL_001.mp4`, "262", "96319", "33")

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
	if stats.VirtualNodes != 1 {
		t.Errorf("VirtualNodes = %d, want 1 (dry run counts would-be virtual nodes)", stats.VirtualNodes)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1 (dry run counts would-be edges)", stats.Emitted)
	}
	if stats.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", stats.Unresolved)
	}
	if stats.NoRewrite != 0 {
		t.Errorf("NoRewrite = %d, want 0", stats.NoRewrite)
	}
}

func TestSyncerRejectsEmptyTimelineID(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `D:\Videos\Norway\PXL_001.mp4`, "262", "96319", "33")

	syncer := &Syncer{DB: db, Index: &fakeIndex{}, DryRun: true}
	_, err = syncer.Sync(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty timeline ID") {
		t.Fatalf("Sync error = %v, want empty timeline ID rejection", err)
	}
}

func TestSyncerEvidenceOnly(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "t1", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `D:\Videos\Norway\PXL_001.mp4`, "262", "96319", "33")

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/videos/Norway/PXL_001.mp4": "node-uuid-001",
	}}

	fakeEmitter := &fakeVirtualEmitter{}
	fakeAttacher := &fakeEdgeAttacher{}

	syncer := &Syncer{
		DB:             db,
		Index:          index,
		AgentID:        "test-agent",
		DatabaseURL:    "postgres://user:secret@localhost:5432/resolve",
		DryRun:         false,
		VirtualEmitter: fakeEmitter,
		Client:         fakeAttacher,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.VirtualNodes != 1 {
		t.Errorf("VirtualNodes = %d, want 1", stats.VirtualNodes)
	}
	if stats.EdgesAttached != 1 {
		t.Errorf("EdgesAttached = %d, want 1", stats.EdgesAttached)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}
	// Verify the virtual node payload.
	if len(fakeEmitter.calls) != 1 {
		t.Fatalf("virtual emitter calls = %d, want 1", len(fakeEmitter.calls))
	}
	call := fakeEmitter.calls[0]
	if call.payload.NodeUUID == "" {
		t.Error("virtual node UUID must not be empty")
	}
	wantVirtualPath := "/virtual/resolve/" + VirtualNodeUUID(syncer.AgentID, "t1", syncer.DatabaseURL)
	if call.payload.FilePath != wantVirtualPath {
		t.Errorf("virtual node FilePath = %q, want %q", call.payload.FilePath, wantVirtualPath)
	}
	if call.payload.DisplayName != "Resolve: Master" {
		t.Errorf("virtual node DisplayName = %q, want %q", call.payload.DisplayName, "Resolve: Master")
	}
	if call.payload.ProjectType != "resolve_project" {
		t.Errorf("virtual node ProjectType = %q, want %q", call.payload.ProjectType, "resolve_project")
	}
	// Verify the edge payload.
	if len(fakeAttacher.calls) != 1 {
		t.Fatalf("edge attacher calls = %d, want 1", len(fakeAttacher.calls))
	}
	edge := fakeAttacher.calls[0]
	if edge.payload.RelationshipType != "PROJECT_SIDECAR" {
		t.Errorf("edge RelationshipType = %q, want %q", edge.payload.RelationshipType, "PROJECT_SIDECAR")
	}
	if edge.payload.Confidence != 1.00 {
		t.Errorf("edge Confidence = %f, want 1.00", edge.payload.Confidence)
	}
	if edge.payload.Tier != 1 {
		t.Errorf("edge Tier = %d, want 1", edge.payload.Tier)
	}
	if edge.payload.Resolver != "resolve_project_db" {
		t.Errorf("edge Resolver = %q, want %q", edge.payload.Resolver, "resolve_project_db")
	}
}

func TestStripCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"postgres with creds", "postgres://admin:REDACTED@localhost:5432/resolve", "postgres://localhost:5432/resolve"},
		{"postgres no creds", "postgres://localhost:5432/resolve", "postgres://localhost:5432/resolve"},
		{"postgresql with creds", "postgresql://user:REDACTED@host/db", "postgresql://host/db"},
		{"file URI", "file:/path/to/db?mode=ro", "file:/path/to/db?mode=ro"},
		{"empty", "", ""},
		{"unparseable", "not-a-url", "not-a-url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripCredentials(tc.in)
			if got != tc.want {
				t.Errorf("stripCredentials(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSyncerEvidenceStripsCredentials(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "t1", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `D:\Videos\Norway\PXL_001.mp4`, "262", "96319", "33")

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/videos/Norway/PXL_001.mp4": "node-uuid-001",
	}}

	// Capture slog output to verify credentials are stripped from evidence.
	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	fakeEmitter := &fakeVirtualEmitter{}
	fakeAttacher := &fakeEdgeAttacher{}

	syncer := &Syncer{
		DB:             db,
		Index:          index,
		AgentID:        "test-agent",
		DatabaseURL:    "postgres://user:REDACTED@localhost:5432/resolve",
		DryRun:         false,
		VirtualEmitter: fakeEmitter,
		Client:         fakeAttacher,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
		Logger: logger,
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.VirtualNodes != 1 {
		t.Fatalf("VirtualNodes = %d, want 1", stats.VirtualNodes)
	}
	if stats.EdgesAttached != 1 {
		t.Fatalf("EdgesAttached = %d, want 1", stats.EdgesAttached)
	}

	output := logBuf.String()
	if strings.Contains(output, "REDACTED") {
		t.Errorf("evidence log contains userinfo — credential leak\nlog output: %s", output)
	}
	if !strings.Contains(output, "resolve-sync: emitted edge") {
		t.Errorf("expected 'resolve-sync: emitted edge' in log output\nlog output: %s", output)
	}
}

func TestSyncerEvidenceCollectsMultipleTimelines(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// Same file in two different timelines.
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master')`); err != nil {
		t.Fatalf("insert timeline: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1')`); err != nil {
		t.Fatalf("insert sequence: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1')`); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '100', '0', '30', 'tr1')`); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	// Resolve permits duplicate display names; stable timeline IDs must keep
	// these as two distinct virtual nodes and memberships.
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES ('t2', 'Master')`); err != nil {
		t.Fatalf("insert timeline 2: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Sequence" VALUES ('seq2', 't2')`); err != nil {
		t.Fatalf("insert sequence 2: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiTrack" VALUES ('tr2', 0, 'seq2')`); err != nil {
		t.Fatalf("insert track 2: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i2', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '200', '30', '30', 'tr2')`); err != nil {
		t.Fatalf("insert item 2: %v", err)
	}

	index := &fakeIndex{entries: map[string]string{
		"/storage/archive/PXL_001.mp4": "node-uuid-001",
	}}
	fakeEmitter := &fakeVirtualEmitter{}
	fakeAttacher := &fakeEdgeAttacher{}

	syncer := &Syncer{
		DB:             db,
		Index:          index,
		Client:         fakeAttacher,
		VirtualEmitter: fakeEmitter,
		AgentID:        "test-agent",
		DatabaseURL:    "file::memory:",
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Same file in two timelines = deduplicated to 1 unique path.
	if stats.ClipsFound != 1 {
		t.Errorf("ClipsFound = %d, want 1 (deduped by path)", stats.ClipsFound)
	}
	// Two unique timelines = 2 virtual nodes.
	if stats.VirtualNodes != 2 {
		t.Errorf("VirtualNodes = %d, want 2 (one per timeline)", stats.VirtualNodes)
	}
	// One edge per unique file/timeline membership.
	if stats.Emitted != 2 {
		t.Errorf("Emitted = %d, want 2", stats.Emitted)
	}
	if stats.EdgesAttached != 2 || len(fakeAttacher.calls) != 2 {
		t.Fatalf("EdgesAttached = %d, calls = %d, want 2 and 2", stats.EdgesAttached, len(fakeAttacher.calls))
	}
	if fakeAttacher.calls[0].payload.TargetNodeUUID == fakeAttacher.calls[1].payload.TargetNodeUUID {
		t.Error("two timeline memberships emitted to the same virtual node")
	}
}

func TestSyncerUnresolvedPath(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "t1", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `D:\Videos\Norway\PXL_001.mp4`, "262", "96319", "33")

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

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	insertClip(t, db, "t1", "Master", "seq1", "tr1", "i1", "PXL_001.mp4", `E:\Other\PXL_001.mp4`, "262", "96319", "33")

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

func TestSyncerDeduplicatesByPath(t *testing.T) {
	db, err := Open(context.Background(), "file::memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.db.ExecContext(context.Background(), resolveSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	// Same file appears in the same timeline twice (different in/out points).
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES ('t1', 'Master')`); err != nil {
		t.Fatalf("insert timeline: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Sequence" VALUES ('seq1', 't1')`); err != nil {
		t.Fatalf("insert sequence: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiTrack" VALUES ('tr1', 0, 'seq1')`); err != nil {
		t.Fatalf("insert track: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i1', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '100', '0', '30', 'tr1')`); err != nil {
		t.Fatalf("insert item 1: %v", err)
	}
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2TiItem" VALUES ('i2', 'PXL_001.mp4', 'D:\Videos\PXL_001.mp4', '200', '30', '30', 'tr1')`); err != nil {
		t.Fatalf("insert item 2: %v", err)
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

// fakeVirtualEmitter captures PostVirtualNodeCreated calls for test assertions.
type fakeVirtualEmitter struct {
	calls []virtualNodeCall
}

type virtualNodeCall struct {
	agentID string
	payload branchdam.VirtualNodeCreated
}

func (f *fakeVirtualEmitter) PostVirtualNodeCreated(ctx context.Context, agentID string, payload branchdam.VirtualNodeCreated) (*branchdam.EventResponse, error) {
	f.calls = append(f.calls, virtualNodeCall{agentID: agentID, payload: payload})
	return &branchdam.EventResponse{EventID: "test-event-id"}, nil
}

// fakeEdgeAttacher captures PostEdgeAttached calls for test assertions.
type fakeEdgeAttacher struct {
	calls []edgeCall
}

type edgeCall struct {
	agentID string
	payload branchdam.EdgeAttachedPayload
}

func (f *fakeEdgeAttacher) PostEdgeAttached(ctx context.Context, agentID string, payload branchdam.EdgeAttachedPayload) (*branchdam.EventResponse, error) {
	f.calls = append(f.calls, edgeCall{agentID: agentID, payload: payload})
	return &branchdam.EventResponse{EventID: "test-event-id"}, nil
}

func TestVirtualNodeUUID_Deterministic(t *testing.T) {
	// Same inputs always produce the same UUID.
	uuid1 := VirtualNodeUUID("agent-1", "timeline-1", "postgres://alice:old-secret@localhost:5432/resolve?sslmode=require")
	uuid2 := VirtualNodeUUID("agent-1", "timeline-1", "postgres://bob:new-secret@localhost:5432/resolve?sslmode=verify-full")
	if uuid1 != uuid2 {
		t.Errorf("VirtualNodeUUID changed with credentials/connection options: %q != %q", uuid1, uuid2)
	}
	// Different stable timeline IDs produce different UUIDs, even when their
	// display names happen to be identical (names are not UUID inputs).
	uuid3 := VirtualNodeUUID("agent-1", "timeline-2", "postgres://localhost:5432/resolve")
	if uuid1 == uuid3 {
		t.Errorf("VirtualNodeUUID should differ for different timelines: %q == %q", uuid1, uuid3)
	}
	// Different database URLs produce different UUIDs.
	uuid4 := VirtualNodeUUID("agent-1", "timeline-1", "postgres://localhost:5433/resolve")
	if uuid1 == uuid4 {
		t.Errorf("VirtualNodeUUID should differ for different databases: %q == %q", uuid1, uuid4)
	}
	// branchDAM's live-path uniqueness is global, so distinct agents syncing
	// the same shared database must emit distinct virtual nodes.
	uuid5 := VirtualNodeUUID("agent-2", "timeline-1", "postgres://localhost:5432/resolve")
	if uuid1 == uuid5 {
		t.Errorf("VirtualNodeUUID should differ for different agents: %q == %q", uuid1, uuid5)
	}
}

func TestVirtualNodeUUID_Format(t *testing.T) {
	uuid := VirtualNodeUUID("agent-1", "test", "file::memory:")
	// UUID v4 format: 8-4-4-4-12 hex digits.
	if len(uuid) != 36 {
		t.Errorf("UUID length = %d, want 36", len(uuid))
	}
	if uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' || uuid[23] != '-' {
		t.Errorf("UUID format wrong: %q", uuid)
	}
	// Version nibble should be 4.
	if uuid[14] != '4' {
		t.Errorf("UUID version = %c, want '4': %q", uuid[14], uuid)
	}
}

func TestVirtualFilePath(t *testing.T) {
	got := virtualFilePath("/virtual/resolve", "1f14bcea-fb5e-4b9f-9b20-447c8cf29167")
	want := "/virtual/resolve/1f14bcea-fb5e-4b9f-9b20-447c8cf29167"
	if got != want {
		t.Errorf("virtualFilePath = %q, want %q", got, want)
	}
}

func TestVirtualDisplayName(t *testing.T) {
	got := virtualDisplayName("Master")
	want := "Resolve: Master"
	if got != want {
		t.Errorf("virtualDisplayName = %q, want %q", got, want)
	}
}
