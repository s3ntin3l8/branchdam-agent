package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPath(t *testing.T) {
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("os.UserConfigDir unavailable in this environment: %v", err)
	}

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join(dir, "branchdam-agent", "config.yaml")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolvePathExplicitFlagWins(t *testing.T) {
	got, err := ResolvePath("/custom/path/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/path/config.yaml" {
		t.Errorf("got %q", got)
	}
}

func TestResolvePathPrefersCWDConfig(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if err := os.WriteFile("config.yaml", []byte("agentId: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "config.yaml" {
		t.Errorf("got %q, want %q", got, "config.yaml")
	}
}

func TestResolvePathFallsBackToDefaultPath(t *testing.T) {
	if _, err := os.UserConfigDir(); err != nil {
		t.Skipf("os.UserConfigDir unavailable in this environment: %v", err)
	}

	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	got, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	want, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(orig) }
}

func TestValidateCatchesUnexpandedPlaceholder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// TEST_UNSET_VAR_XYZ is deliberately never set.
	content := `
server:
  baseUrl: "http://localhost:8080"
  apiKey: "${TEST_UNSET_VAR_XYZ}"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Sanity: today's naive `!= ""` check would pass here -- that's the
	// exact footgun Validate exists to catch instead.
	if cfg.Server.APIKey == "" {
		t.Fatal("test setup broken: APIKey should be the literal placeholder, not empty")
	}

	problems := cfg.Validate()
	found := false
	for _, p := range problems {
		if p.Field == "server.apiKey" {
			found = true
			// server.apiKey is the one field checkPlaceholder redacts --
			// CodeQL's go/clear-text-logging flagged the unredacted form
			// once a Problem's message started reaching slog (the tray),
			// not just fmt.Fprintln (preflight): the matched substring is
			// safe when the value really is the literal placeholder, but
			// nothing here can prove that for every possible apiKey
			// value, so the placeholder text itself must never appear in
			// the message for this field.
			if strings.Contains(p.Message, "TEST_UNSET_VAR_XYZ") {
				t.Errorf("server.apiKey's problem message must not echo the matched placeholder text, got %q", p.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected Validate to flag server.apiKey's unexpanded placeholder, got %v", problems)
	}
}

// TestValidateNonSensitiveFieldNamesThePlaceholder is the flip side of the
// redaction above: for a field that isn't a secret, naming the actual
// unresolved ${VAR} is useful diagnostic information and must survive.
func TestValidateNonSensitiveFieldNamesThePlaceholder(t *testing.T) {
	cfg := Config{Ingest: IngestConfig{ArchiveRoot: "${TEST_UNSET_VAR_XYZ}"}}
	problems := cfg.Validate()
	found := false
	for _, p := range problems {
		if p.Field == "ingest.archiveRoot" {
			found = true
			if !strings.Contains(p.Message, "${TEST_UNSET_VAR_XYZ}") {
				t.Errorf("expected ingest.archiveRoot's message to name the placeholder, got %q", p.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected Validate to flag ingest.archiveRoot's unexpanded placeholder, got %v", problems)
	}
}

func TestValidateNoProblemsOnWellFormedConfig(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{
			BaseURL: "http://localhost:8080",
			APIKey:  "0123456789abcdef0123456789abcdef",
		},
		AgentID: "workstation-01",
		Ingest: IngestConfig{
			ArchiveRoot:   "/archive",
			LocalEditRoot: "/edit",
		},
	}
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Errorf("expected no problems, got %v", problems)
	}
}

func TestValidateShortAPIKey(t *testing.T) {
	cfg := Config{Server: ServerConfig{APIKey: "too-short"}}
	problems := cfg.Validate()
	found := false
	for _, p := range problems {
		if p.Field == "server.apiKey" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a problem for a too-short apiKey, got %v", problems)
	}
}

func TestValidateNegativeIntervals(t *testing.T) {
	cfg := Config{
		Ingest: IngestConfig{PollIntervalSecs: -1},
		Prune:  PruneConfig{MinAgeHours: -1},
	}
	problems := cfg.Validate()
	if len(problems) != 2 {
		t.Errorf("expected 2 problems, got %v", problems)
	}
}
