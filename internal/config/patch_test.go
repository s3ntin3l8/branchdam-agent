package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// commentLines returns every trimmed comment line ("# ..." with leading
// whitespace stripped) in s, in order. Used to assert Patch doesn't drop or
// reorder comments -- exact byte-for-byte reindentation isn't the property
// under test, comment survival and ordering is.
func commentLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			out = append(out, trimmed)
		}
	}
	return out
}

// TestPatchPreservesCommentsAndUnexpandedPlaceholders is the golden test
// this package's whole write-back strategy hinges on: a yaml.Marshal(cfg)
// round-trip would destroy config.example.yaml's comments and bake
// server.apiKey's ${VAR} placeholder into its (here, unset-so-literal)
// expanded form. Patch must do neither, touching only the key it was asked
// to change.
func TestPatchPreservesCommentsAndUnexpandedPlaceholders(t *testing.T) {
	original, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml fixture: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, map[string]any{"tray.startOnLogin": true}); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	patched, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	wantComments := commentLines(string(original))
	gotComments := commentLines(string(patched))
	if len(wantComments) != len(gotComments) {
		t.Fatalf("comment count changed: got %d, want %d\ngot: %v\nwant: %v",
			len(gotComments), len(wantComments), gotComments, wantComments)
	}
	for i := range wantComments {
		if gotComments[i] != wantComments[i] {
			t.Errorf("comment %d changed:\n got:  %q\n want: %q", i, gotComments[i], wantComments[i])
		}
	}

	if !strings.Contains(string(patched), `${BRANCHDAM_AGENT_API_KEY}`) {
		t.Error("apiKey's ${VAR} placeholder was expanded or lost -- Patch must never bake in Load()'s env expansion")
	}

	// Parse the patched file the same way Load's YAML step does (no env
	// expansion here on purpose -- we're asserting what's on disk, not
	// what an operator's process environment happens to resolve it to).
	var cfg Config
	if err := yaml.Unmarshal(patched, &cfg); err != nil {
		t.Fatalf("patched file is not valid YAML: %v", err)
	}

	if !cfg.Tray.StartOnLogin {
		t.Error("patched field tray.startOnLogin did not take effect")
	}
	if cfg.Server.APIKey != "${BRANCHDAM_AGENT_API_KEY}" {
		t.Errorf("server.apiKey changed unexpectedly: got %q", cfg.Server.APIKey)
	}
	if cfg.Ingest.ArchiveRoot != `D:\Photos\Archive` {
		t.Errorf("unrelated field ingest.archiveRoot changed: got %q", cfg.Ingest.ArchiveRoot)
	}
	if cfg.SelfUpdate.CheckIntervalHours != 24 {
		t.Errorf("unrelated field selfUpdate.checkIntervalHours changed: got %d", cfg.SelfUpdate.CheckIntervalHours)
	}
	if cfg.Prune.MinAgeHours != 24 {
		t.Errorf("unrelated field prune.minAgeHours changed: got %d", cfg.Prune.MinAgeHours)
	}
}

func TestPatchWritesAtomicallyWithRestrictedMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  baseUrl: http://x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, map[string]any{"server.apiKey": "super-secret"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected mode 0600 after Patch (config now may hold a plaintext secret), got %o", perm)
	}

	// No leftover temp files in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("unexpected leftover file after Patch: %s", e.Name())
		}
	}
}

func TestPatchCreatesMissingNestedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Patch(path, map[string]any{
		"tray.startOnLogin":       true,
		"tray.statusAddr":         "127.0.0.1:9999",
		"ingest.cardRoots":        []string{"/media/alice", "/run/media/alice"},
		"selfUpdate.enabled":      false,
		"server.baseUrl":          "https://branchdam.example.com",
		"ingest.pollIntervalSecs": 5,
	})
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load patched (previously empty) config: %v", err)
	}
	if !cfg.Tray.StartOnLogin || cfg.Tray.StatusAddr != "127.0.0.1:9999" {
		t.Errorf("tray fields not applied: %+v", cfg.Tray)
	}
	if len(cfg.Ingest.CardRoots) != 2 || cfg.Ingest.CardRoots[0] != "/media/alice" {
		t.Errorf("ingest.cardRoots not applied: %v", cfg.Ingest.CardRoots)
	}
	if cfg.SelfUpdate.Enabled {
		t.Error("selfUpdate.enabled not applied")
	}
	if cfg.Server.BaseURL != "https://branchdam.example.com" {
		t.Errorf("server.baseUrl not applied: %q", cfg.Server.BaseURL)
	}
	if cfg.Ingest.PollIntervalSecs != 5 {
		t.Errorf("ingest.pollIntervalSecs not applied: %d", cfg.Ingest.PollIntervalSecs)
	}
}

func TestPatchOnExistingLeafPreservesLineComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server:\n  baseUrl: http://x # inline note\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, map[string]any{"server.baseUrl": "https://y"}); err != nil {
		t.Fatalf("Patch: %v", err)
	}

	patched, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "# inline note") {
		t.Errorf("inline comment on patched leaf was dropped, got:\n%s", patched)
	}
}

func TestPatchRejectsNonMappingRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("- just\n- a\n- list\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Patch(path, map[string]any{"server.baseUrl": "https://y"}); err == nil {
		t.Error("expected an error patching a non-mapping document root")
	}
}
