package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
)

// literalKey is the same fixture key the rest of the repo already uses.
const literalKey = "0123456789abcdef0123456789abcdef" // pragma: allowlist secret

func TestReadTools(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "queue.db")
	seedQueue(t, dbPath)

	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(dbPath, past, past); err != nil {
		t.Fatal(err)
	}
	versionBefore := userVersion(t, dbPath)
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	opts := Options{QueuePath: dbPath, ConfigPath: writeConfig(t, dir, "apiKey: "+literalKey+"\n")}
	summary := callTool(t, opts, "queue_summary", map[string]any{})
	if summary.IsError {
		t.Fatalf("queue_summary: %s", toolText(summary))
	}
	body := structured(t, summary)
	if num(body["done"]) != 1 || num(body["failed"]) != 1 || num(body["awaitingUpload"]) != 0 {
		t.Fatalf("counts = %#v", body)
	}

	failed := callTool(t, opts, "list_failed", map[string]any{"limit": 10})
	if failed.IsError {
		t.Fatalf("list_failed: %s", toolText(failed))
	}
	rows := structured(t, failed)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("failed rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["nodeUuid"] != "failed-1" || row["step"] != "archive_copy" || row["lastError"] != "archive conflict" {
		t.Fatalf("failed row = %#v", row)
	}

	for _, args := range []map[string]any{
		{"nodeUuid": "done-1"},
		{"sourcePath": "/card/done.jpg"},
		{"localPath": "/local/done.jpg"},
	} {
		got := callTool(t, opts, "lookup_queue_row", args)
		if got.IsError {
			t.Fatalf("lookup %v: %s", args, toolText(got))
		}
		found := structured(t, got)
		if found["found"] != true {
			t.Fatalf("lookup %v found = %#v", args, found)
		}
		looked := found["row"].(map[string]any)
		if looked["nodeUuid"] != "done-1" {
			t.Fatalf("lookup %v row = %#v", args, looked)
		}
	}

	both := callTool(t, opts, "lookup_queue_row", map[string]any{
		"nodeUuid": "done-1", "sourcePath": "/card/done.jpg",
	})
	if !both.IsError {
		t.Fatal("lookup with two keys succeeded")
	}

	var opened int
	bad := callTool(t, Options{
		QueuePath: dbPath,
		OpenQueue: func(path string) (*queue.Store, error) {
			opened++
			return queue.OpenReadOnly(path)
		},
	}, "lookup_queue_row", map[string]any{})
	if !bad.IsError || opened != 0 {
		t.Fatalf("empty lookup isError=%v opened=%d text=%s", bad.IsError, opened, toolText(bad))
	}

	missing := callTool(t, Options{}, "queue_summary", map[string]any{})
	if !missing.IsError || !strings.Contains(toolText(missing), "not configured") {
		t.Fatalf("missing queue: %s", toolText(missing))
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("queue.db mtime changed from %s to %s", before.ModTime(), after.ModTime())
	}
	if got := userVersion(t, dbPath); got != versionBefore {
		t.Fatalf("user_version = %d, want %d", got, versionBefore)
	}
}

func TestAgentConfigRedactsKey(t *testing.T) {
	dir := t.TempDir()
	path := writeConfig(t, dir, "server:\n  apiKey: "+literalKey+"\n  baseUrl: http://127.0.0.1:8080\n")
	res := callTool(t, Options{ConfigPath: path}, "agent_config", map[string]any{})
	if res.IsError {
		t.Fatal(toolText(res))
	}
	raw := mustJSON(t, structured(t, res))
	if strings.Contains(raw, literalKey) {
		t.Fatalf("literal key leaked: %s", raw)
	}
	server := structured(t, res)["server"].(map[string]any)
	if server["apiKey"] != redactedKey {
		t.Fatalf("apiKey = %#v", server["apiKey"])
	}

	t.Setenv("TEST_API_KEY", literalKey)
	path = writeConfig(t, dir, "server:\n  apiKey: ${TEST_API_KEY}\n  baseUrl: http://127.0.0.1:8080\n")
	res = callTool(t, Options{ConfigPath: path}, "agent_config", map[string]any{})
	if res.IsError {
		t.Fatal(toolText(res))
	}
	raw = mustJSON(t, structured(t, res))
	if strings.Contains(raw, literalKey) {
		t.Fatalf("expanded placeholder leaked: %s", raw)
	}
	server = structured(t, res)["server"].(map[string]any)
	if server["apiKey"] != "${TEST_API_KEY}" {
		t.Fatalf("placeholder apiKey = %#v", server["apiKey"])
	}
}

func TestPreflightTool(t *testing.T) {
	res := callTool(t, Options{
		Preflight: func(context.Context) ([]CheckResult, bool, error) {
			return []CheckResult{NewCheck("server", "OK", "server reachable")}, true, nil
		},
	}, "preflight", map[string]any{})
	if res.IsError {
		t.Fatal(toolText(res))
	}
	body := structured(t, res)
	if body["ok"] != true {
		t.Fatalf("ok = %#v", body["ok"])
	}
	checks := body["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["detail"] != "server reachable" {
		t.Fatalf("checks = %#v", checks)
	}
}

func seedQueue(t *testing.T, path string) {
	t.Helper()
	s, err := queue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.InsertPending(ctx, mediaRecord("done-1", "/card/done.jpg", "/local/done.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyDone(ctx, "done-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRebaseDone(ctx, "done-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPending(ctx, mediaRecord("failed-1", "/card/failed.jpg", "/local/failed.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveCopyFailed(ctx, "failed-1", "archive conflict"); err != nil {
		t.Fatal(err)
	}
}

func mediaRecord(nodeUUID, source, local string) queue.NewRecord {
	return queue.NewRecord{
		NodeUUID:             nodeUUID,
		Kind:                 queue.KindMedia,
		SourcePath:           source,
		LocalPath:            local,
		ArchivePath:          "/archive/" + nodeUUID + ".jpg",
		ArchiveContainerPath: "/storage/archive/" + nodeUUID + ".jpg",
		FileName:             nodeUUID + ".jpg",
		FileExt:              "jpg",
		SizeBytes:            100,
		FullHash:             "abc",
		FastHash:             "def",
	}
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func callTool(t *testing.T, opts Options, name string, args map[string]any) *sdkmcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if opts.OpenQueue == nil {
		opts.OpenQueue = queue.OpenReadOnly
	}
	serverTransport, clientTransport := sdkmcp.NewInMemoryTransports()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "branchdam-agent", Version: "test"}, nil)
	registerTools(server, opts)
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func toolText(res *sdkmcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*sdkmcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func structured(t *testing.T, res *sdkmcp.CallToolResult) map[string]any {
	t.Helper()
	if m, ok := res.StructuredContent.(map[string]any); ok {
		return m
	}
	body, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("structured %T: %s", res.StructuredContent, body)
	}
	return m
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return -1
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}
