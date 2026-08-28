package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/luminar"
)

// createTestCatalog builds a minimal catalog on Luminar Neo's real schema
// (see internal/luminar.DefaultCatalogQuery / docs/luminar-catalog.md), with
// exactly one image pair: a source and its _upscale derivative.
func createTestCatalog(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer func() { _ = db.Close() }()

	stmts := []string{
		`CREATE TABLE volumes (_id_int_64 INTEGER PRIMARY KEY, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, guid_wide_ch TEXT NOT NULL DEFAULT '', info_wide_ch TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE paths (_id_int_64 INTEGER PRIMARY KEY, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, volume_id_int_64 INTEGER NOT NULL, path_wide_ch TEXT NOT NULL)`,
		`CREATE TABLE images (_id_int_64 INTEGER PRIMARY KEY, guid_wide_ch TEXT NOT NULL UNIQUE, path_wide_ch TEXT NOT NULL, creation_date_int_64 INTEGER NOT NULL DEFAULT 0, marked_to_delete_bool INTEGER NOT NULL DEFAULT 0, deleted_at_int_64 INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE paths_images (_key_id_int_64 INTEGER NOT NULL, _val_id_int_64 INTEGER NOT NULL, PRIMARY KEY (_key_id_int_64, _val_id_int_64))`,
		`CREATE TABLE image_user_attributes (_id_int_64 INTEGER PRIMARY KEY, _out_id_int_64 INTEGER NOT NULL UNIQUE, trash_bool INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE image_exiv_attributes (_id_int_64 INTEGER PRIMARY KEY, _out_id_int_64 INTEGER NOT NULL UNIQUE, camera_model_wide_ch TEXT, date_time_int_64 INTEGER NOT NULL DEFAULT 0)`,

		`INSERT INTO volumes (_id_int_64, guid_wide_ch, info_wide_ch) VALUES (1, 'v1', '{"kMountPointSerializationKey":"/masters"}')`,
		`INSERT INTO paths (_id_int_64, volume_id_int_64, path_wide_ch) VALUES (10, 1, '')`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch) VALUES (1, 'g1', 'DSC_0001.NEF')`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch) VALUES (2, 'g2', 'DSC_0001_upscale.jpg')`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 1)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 2)`,
		// A second pair using a suffix ("_hdr") NOT in the built-in default
		// list -- exists purely so a -derivative-suffixes test can prove the
		// flag actually ADDS a non-default suffix through the CLI, not just
		// that it can narrow the default list.
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch) VALUES (3, 'g3', 'DSC_0002.NEF')`,
		`INSERT INTO images (_id_int_64, guid_wide_ch, path_wide_ch) VALUES (4, 'g4', 'DSC_0002_hdr.jpg')`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 3)`,
		`INSERT INTO paths_images (_key_id_int_64, _val_id_int_64) VALUES (10, 4)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
	return path
}

func writeNodeIndexFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "node-index.json")
	content := `{
		"/masters/DSC_0001.NEF": "0198f2c1-2e3a-7c9e-8b1a-000000000001",
		"/masters/DSC_0001_upscale.jpg": "0198f2c1-2e3a-7c9e-8b1a-000000000002"
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write node index: %v", err)
	}
	return path
}

func TestRunLuminarSyncMissingCatalogFlag(t *testing.T) {
	if got := run([]string{"luminar-sync"}); got != 2 {
		t.Errorf("run([luminar-sync]) with no -catalog = %d, want 2", got)
	}
}

func TestRunLuminarSyncDumpSchema(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	got := run([]string{"luminar-sync", "-catalog", catalogPath, "-dump-schema"})
	if got != 0 {
		t.Errorf("run([luminar-sync -dump-schema]) = %d, want 0", got)
	}
}

func TestRunLuminarSyncDryRun(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	got := run([]string{
		"luminar-sync",
		"-catalog", catalogPath,
		"-node-index", nodeIndexPath,
		"-dry-run",
	})
	if got != 0 {
		t.Errorf("run([luminar-sync -dry-run]) = %d, want 0", got)
	}
}

// TestRunLuminarSyncAgainstRealHTTPServer exercises the full stack end to
// end -- catalog read, derivative pairing, node-index resolution, JSON
// marshalling, and the real branchdam.Client.PostEdgeAttached over HTTP --
// against a fake /api/v1/agent/events handler that decodes the
// double-encoded payload and asserts tier/confidence/relationshipType/
// resolver exactly as branchDAM's applyEdgeAttached would read them.
func TestRunLuminarSyncAgainstRealHTTPServer(t *testing.T) {
	var gotEnvelope branchdam.EventEnvelope
	var gotPayload branchdam.EdgeAttachedPayload

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != "0123456789abcdef0123456789abcdef" {
			t.Errorf("X-API-Key = %q, want the configured key", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotEnvelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if gotEnvelope.EventType != branchdam.EventEdgeAttached {
			t.Errorf("eventType = %q, want %q", gotEnvelope.EventType, branchdam.EventEdgeAttached)
		}
		// Payload must be a JSON *string* (double-encoded), not an object.
		if err := json.Unmarshal([]byte(gotEnvelope.Payload), &gotPayload); err != nil {
			t.Fatalf("unmarshal double-encoded payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-test"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	cfgPath := filepath.Join(dir, "config.yaml")
	cfgContent := "server:\n  baseUrl: \"" + srv.URL + "\"\n  apiKey: \"0123456789abcdef0123456789abcdef\"\nagentId: \"test-agent\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{
		"luminar-sync",
		"-config", cfgPath,
		"-catalog", catalogPath,
		"-node-index", nodeIndexPath,
	})
	if got != 0 {
		t.Fatalf("run([luminar-sync]) = %d, want 0", got)
	}

	if gotEnvelope.AgentID != "test-agent" {
		t.Errorf("envelope.AgentID = %q, want test-agent", gotEnvelope.AgentID)
	}
	if gotPayload.SourceNodeUUID != "0198f2c1-2e3a-7c9e-8b1a-000000000001" {
		t.Errorf("SourceNodeUUID = %q, want the master's uuid", gotPayload.SourceNodeUUID)
	}
	if gotPayload.TargetNodeUUID != "0198f2c1-2e3a-7c9e-8b1a-000000000002" {
		t.Errorf("TargetNodeUUID = %q, want the edit's uuid", gotPayload.TargetNodeUUID)
	}
	if gotPayload.RelationshipType != branchdam.RelationshipDerivedFrom {
		t.Errorf("RelationshipType = %q, want %q", gotPayload.RelationshipType, branchdam.RelationshipDerivedFrom)
	}
	if gotPayload.Tier != 2 {
		t.Errorf("Tier = %d, want 2", gotPayload.Tier)
	}
	if gotPayload.Confidence != 0.89 {
		t.Errorf("Confidence = %v, want 0.89", gotPayload.Confidence)
	}
	if gotPayload.Resolver != "luminar_catalog" {
		t.Errorf("Resolver = %q, want luminar_catalog", gotPayload.Resolver)
	}
}

// TestRunLuminarSyncQueryFileOverride proves -query-file actually threads
// through runLuminarSyncCmd into the syncer, not just Syncer.Query in
// isolation (sync_test.go covers that half already). The override query
// below intentionally ignores the fixture catalog's real rows and returns a
// different, hardcoded row with different column aliases -- if the flag
// weren't wired up, DefaultCatalogQuery's real row would be read instead and
// this assertion would catch it.
func TestRunLuminarSyncQueryFileOverride(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	queryFilePath := filepath.Join(dir, "override.sql")
	overrideQuery := `
SELECT '10' AS whatever_id, '/override' AS whatever_mount, 'dir' AS whatever_dir, 'OVERRIDE.NEF' AS whatever_file, 0 AS whatever_trash, '' AS whatever_cam, 0 AS whatever_time
UNION ALL
SELECT '11', '/override', 'dir', 'OVERRIDE_upscale.jpg', 0, '', 0
`
	if err := os.WriteFile(queryFilePath, []byte(overrideQuery), 0o600); err != nil {
		t.Fatal(err)
	}

	nodeIndexPath := filepath.Join(dir, "node-index.json")
	nodeIndexContent := `{
		"/override/dir/OVERRIDE.NEF": "0198f2c1-2e3a-7c9e-8b1a-0000000000aa",
		"/override/dir/OVERRIDE_upscale.jpg": "0198f2c1-2e3a-7c9e-8b1a-0000000000bb"
	}`
	if err := os.WriteFile(nodeIndexPath, []byte(nodeIndexContent), 0o600); err != nil {
		t.Fatal(err)
	}

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
		_, _ = w.Write([]byte(`{"eventId":"evt-override"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfgPath := filepath.Join(dir, "config.yaml")
	cfgContent := "server:\n  baseUrl: \"" + srv.URL + "\"\n  apiKey: \"0123456789abcdef0123456789abcdef\"\nagentId: \"test-agent\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	got := run([]string{
		"luminar-sync",
		"-config", cfgPath,
		"-catalog", catalogPath,
		"-node-index", nodeIndexPath,
		"-query-file", queryFilePath,
	})
	if got != 0 {
		t.Fatalf("run([luminar-sync -query-file]) = %d, want 0", got)
	}

	if gotPayload.SourceNodeUUID != "0198f2c1-2e3a-7c9e-8b1a-0000000000aa" {
		t.Errorf("SourceNodeUUID = %q, want the override query's uuid -- -query-file did not take effect", gotPayload.SourceNodeUUID)
	}
	if gotPayload.TargetNodeUUID != "0198f2c1-2e3a-7c9e-8b1a-0000000000bb" {
		t.Errorf("TargetNodeUUID = %q, want the override query's uuid -- -query-file did not take effect", gotPayload.TargetNodeUUID)
	}
}

// TestRunLuminarSyncDerivativeSuffixesNarrowsDefault proves
// -derivative-suffixes threads through into the syncer in the "remove a
// default" direction: the fixture's DSC_0001 pair uses "_upscale", the
// built-in default, so overriding with a list that excludes it must find 0
// pairs. This alone wouldn't catch a broken flag (a no-op override falls
// back to DefaultDerivativeSuffixes, which still includes "_upscale" and
// would also produce >0 pairs) -- paired with
// TestRunLuminarSyncDerivativeSuffixesAddsSuffix below, which proves the
// opposite direction, a broken split/threading path fails at least one.
func TestRunLuminarSyncDerivativeSuffixesNarrowsDefault(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	var got int
	stdout := captureStdout(t, func() {
		got = run([]string{
			"luminar-sync",
			"-catalog", catalogPath,
			"-node-index", nodeIndexPath,
			"-derivative-suffixes", "_panorama",
			"-dry-run",
		})
	})
	if got != 0 {
		t.Fatalf("run([luminar-sync -derivative-suffixes]) = %d, want 0", got)
	}

	if !strings.Contains(stdout, "0 pair(s) found") {
		t.Errorf("stdout = %q, want it to report 0 pairs found (the fixture's DSC_0001 pair uses _upscale, not in the override list)", stdout)
	}
}

// TestRunLuminarSyncDerivativeSuffixesAddsSuffix proves the flag can ADD a
// suffix the built-in default list does NOT contain -- the actual
// justification for the flag existing. Uses the fixture's DSC_0002/_hdr
// pair, which no default-suffix run would ever find.
func TestRunLuminarSyncDerivativeSuffixesAddsSuffix(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	nodeIndexPath := filepath.Join(dir, "node-index.json")
	nodeIndexContent := `{
		"/masters/DSC_0002.NEF": "0198f2c1-2e3a-7c9e-8b1a-000000000003",
		"/masters/DSC_0002_hdr.jpg": "0198f2c1-2e3a-7c9e-8b1a-000000000004"
	}`
	if err := os.WriteFile(nodeIndexPath, []byte(nodeIndexContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var got int
	stdout := captureStdout(t, func() {
		got = run([]string{
			"luminar-sync",
			"-catalog", catalogPath,
			"-node-index", nodeIndexPath,
			"-derivative-suffixes", "_hdr",
			"-dry-run",
		})
	})
	if got != 0 {
		t.Fatalf("run([luminar-sync -derivative-suffixes]) = %d, want 0", got)
	}

	if !strings.Contains(stdout, "1 pair(s) found") || !strings.Contains(stdout, "1 emitted") {
		t.Errorf("stdout = %q, want 1 pair found and 1 emitted -- -derivative-suffixes did not add _hdr", stdout)
	}
}

func TestRunLuminarSyncMissingNodeIndexFlag(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	got := run([]string{"luminar-sync", "-catalog", catalogPath})
	if got != 2 {
		t.Errorf("run([luminar-sync]) with no -node-index = %d, want 2", got)
	}
}

// failingWriter always errors, for covering runDumpSchema's write-error
// branch (e.g. stdout closed by the caller mid-write).
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("simulated write failure")
}

func TestRunDumpSchemaCatalogError(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	cat, err := luminar.Open(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Closing the catalog before DumpSchema forces its query to fail,
	// covering runDumpSchema's error branch directly rather than only its
	// happy path (already covered by TestRunLuminarSyncDumpSchema).
	if err := cat.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var buf bytes.Buffer
	got := runDumpSchema(context.Background(), &buf, cat)
	if got != 1 {
		t.Errorf("runDumpSchema against a closed catalog = %d, want 1", got)
	}
}

func TestRunDumpSchemaWriteError(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	cat, err := luminar.Open(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cat.Close() }()

	got := runDumpSchema(context.Background(), failingWriter{}, cat)
	if got != 1 {
		t.Errorf("runDumpSchema with a failing writer = %d, want 1", got)
	}
}
