package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// createTestCatalog builds a minimal catalog.db shaped like
// internal/luminar.DefaultEditSourceQuery's guessed schema, with exactly one
// exported edit.
func createTestCatalog(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer func() { _ = db.Close() }()

	stmts := []string{
		`CREATE TABLE ZASSET (Z_PK INTEGER PRIMARY KEY, ZFILEPATH TEXT)`,
		`CREATE TABLE ZEDIT (Z_PK INTEGER PRIMARY KEY, ZASSET INTEGER, ZEXPORTPATH TEXT)`,
		`INSERT INTO ZASSET (Z_PK, ZFILEPATH) VALUES (1, '/masters/DSC_0001.NEF')`,
		`INSERT INTO ZEDIT (Z_PK, ZASSET, ZEXPORTPATH) VALUES (1, 1, '/exports/DSC_0001-edit.jpg')`,
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
		"/exports/DSC_0001-edit.jpg": "0198f2c1-2e3a-7c9e-8b1a-000000000002"
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
// end -- catalog read, node-index resolution, JSON marshalling, and the real
// branchdam.Client.PostEdgeAttached over HTTP -- against a fake
// /api/v1/agent/events handler that decodes the double-encoded payload and
// asserts tier/confidence/relationshipType/resolver exactly as branchDAM's
// applyEdgeAttached would read them.
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
// below intentionally ignores the fixture catalog's real ZASSET/ZEDIT rows
// and returns a different, hardcoded pair with different column aliases --
// if the flag weren't wired up, DefaultEditSourceQuery's real pair would be
// emitted instead and this assertion would catch it.
func TestRunLuminarSyncQueryFileOverride(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	queryFilePath := filepath.Join(dir, "override.sql")
	overrideQuery := `SELECT '/masters/OVERRIDE.NEF' AS whatever_source, '/exports/OVERRIDE.jpg' AS whatever_edit, 'override-src-id' AS src_id, 'override-edit-id' AS edit_id`
	if err := os.WriteFile(queryFilePath, []byte(overrideQuery), 0o600); err != nil {
		t.Fatal(err)
	}

	nodeIndexPath := filepath.Join(dir, "node-index.json")
	nodeIndexContent := `{
		"/masters/OVERRIDE.NEF": "0198f2c1-2e3a-7c9e-8b1a-0000000000aa",
		"/exports/OVERRIDE.jpg": "0198f2c1-2e3a-7c9e-8b1a-0000000000bb"
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

func TestRunLuminarSyncMissingNodeIndexFlag(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)

	got := run([]string{"luminar-sync", "-catalog", catalogPath})
	if got != 2 {
		t.Errorf("run([luminar-sync]) with no -node-index = %d, want 2", got)
	}
}
