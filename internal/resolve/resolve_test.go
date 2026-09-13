package resolve

import (
	"context"
	"log/slog"
	"strings"
	"testing"
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

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "postgres://user:secret@localhost:5432/resolve",
		DryRun:      false,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.EvidenceOnly != 1 {
		t.Errorf("EvidenceOnly = %d, want 1", stats.EvidenceOnly)
	}
	if stats.Emitted != 0 {
		t.Errorf("Emitted = %d, want 0 (self-edges not emitted)", stats.Emitted)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
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

	syncer := &Syncer{
		DB:          db,
		Index:       index,
		AgentID:     "test-agent",
		DatabaseURL: "postgres://user:REDACTED@localhost:5432/resolve",
		DryRun:      false,
		PathRewrites: []PathRewrite{
			{From: "D:\\Videos\\", To: "/storage/archive/videos/"},
		},
		Logger: logger,
	}

	stats, err := syncer.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if stats.EvidenceOnly != 1 {
		t.Fatalf("EvidenceOnly = %d, want 1", stats.EvidenceOnly)
	}

	output := logBuf.String()
	if strings.Contains(output, "REDACTED") {
		t.Errorf("evidence log contains userinfo — credential leak\nlog output: %s", output)
	}
	if !strings.Contains(output, "resolve-sync: resolve evidence") {
		t.Errorf("expected 'resolve-sync: resolve evidence' in log output\nlog output: %s", output)
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
	if _, err := db.db.ExecContext(context.Background(), `INSERT INTO "Sm2Timeline" VALUES ('t2', 'YouTube')`); err != nil {
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
	// Same file in two timelines = deduplicated to 1 unique path.
	if stats.ClipsFound != 1 {
		t.Errorf("ClipsFound = %d, want 1 (deduped by path)", stats.ClipsFound)
	}
	if stats.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", stats.Emitted)
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
