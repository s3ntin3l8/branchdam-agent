package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// noopIngester satisfies tray.Ingester without touching a real card or
// server -- these tests are about configSettings' own logic (persistence,
// reload, restart-required diffing), not ingest behavior.
type noopIngester struct{}

func (noopIngester) IngestCard(_ context.Context, _ string) (ingest.CardResult, error) {
	return ingest.CardResult{}, nil
}

func settingsTestFixture(t *testing.T) (path string, cfg config.Config, runner *tray.Runner) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"http://localhost:8080\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + filepath.Join(dir, "archive") + "\"\n" +
		"  localEditRoot: \"" + filepath.Join(dir, "local") + "\"\n" +
		"  cardRoots:\n" +
		"    - \"" + filepath.Join(dir, "cards") + "\"\n" +
		"tray:\n" +
		"  statusAddr: \"127.0.0.1:38080\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner = tray.NewRunner(noopIngester{}, cfg.Ingest.CardRoots, cfg.Ingest.LocalEditRoot)
	return path, cfg, runner
}

// editConfigFile does a literal string substitution directly on
// config.yaml, bypassing configSettings entirely -- simulating an
// operator's hand-edit, which Reload (and its RestartRequired diffing)
// must handle exactly as well as a menu-driven change.
func editConfigFile(t *testing.T, path, old, new string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), old, new, 1)
	if edited == string(content) {
		t.Fatalf("editConfigFile: %q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigSettingsSnapshot(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	sv := s.Snapshot()
	if sv.ConfigPath != path {
		t.Errorf("ConfigPath = %q, want %q", sv.ConfigPath, path)
	}
	if sv.ServerBaseURL != "http://localhost:8080" {
		t.Errorf("ServerBaseURL = %q", sv.ServerBaseURL)
	}
	if !sv.ServerAPIKeySet {
		t.Error("expected ServerAPIKeySet=true")
	}
	if sv.RestartRequired {
		t.Error("expected RestartRequired=false on a fresh snapshot")
	}
}

func TestConfigSettingsSetBoolPersistsAndReloads(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	if err := s.SetBool("ingest.requireUnbuffered", true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}

	if !s.Snapshot().RequireUnbuffered {
		t.Error("expected Snapshot to reflect the change immediately")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Ingest.RequireUnbuffered {
		t.Error("expected the change to be persisted to disk")
	}
}

func TestConfigSettingsSetIntPersistsAndReloads(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	if err := s.SetInt("selfUpdate.checkIntervalHours", -1); err != nil {
		t.Fatalf("SetInt: %v", err)
	}
	if got := s.Snapshot().SelfUpdateCheckIntervalHrs; got != -1 {
		t.Errorf("SelfUpdateCheckIntervalHrs = %d, want -1", got)
	}
}

func TestConfigSettingsPromptAndSetHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	var gotArgs []string
	dialog := func(args ...string) (string, int, error) {
		gotArgs = args
		return "https://branchdam.example.com", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldServerBaseURL)
	if err != nil {
		t.Fatalf("PromptAndSet: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.Snapshot().ServerBaseURL != "https://branchdam.example.com" {
		t.Errorf("ServerBaseURL = %q", s.Snapshot().ServerBaseURL)
	}
	if !slices.Contains(gotArgs, "entry") {
		t.Errorf("expected -kind entry in dialog args, got %v", gotArgs)
	}
}

func TestConfigSettingsPromptAndSetCanceled(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := func(args ...string) (string, int, error) {
		return "", dialogExitCanceled, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldArchiveRoot)
	if err != nil {
		t.Fatalf("expected no error on cancel, got %v", err)
	}
	if ok {
		t.Error("expected ok=false on cancel")
	}
	if s.Snapshot().ArchiveRoot != cfg.Ingest.ArchiveRoot {
		t.Error("expected ArchiveRoot unchanged after a canceled prompt")
	}
}

func TestConfigSettingsPromptAndSetDialogFailure(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := func(args ...string) (string, int, error) {
		return "", dialogExitFailed, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldNamingTemplate)
	if err == nil {
		t.Fatal("expected an error when the dialog fails to render")
	}
	if ok {
		t.Error("expected ok=false on failure")
	}
}

func TestConfigSettingsAPIKeyNeverPassedAsDialogDefault(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	var gotArgs []string
	dialog := func(args ...string) (string, int, error) {
		gotArgs = args
		return "new-key-value-0123456789abcdef0123456789", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	if _, err := s.PromptAndSet(tray.FieldServerAPIKey); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(gotArgs, "-default") {
		t.Fatalf("API key prompt must never pass a -default (would put the old secret in argv), got %v", gotArgs)
	}
}

func TestConfigSettingsReloadDetectsRestartRequiredStatusAddr(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	// Hand-edit statusAddr directly (bypassing SetBool/SetInt/PromptAndSet
	// entirely -- there is no menu path for this field on purpose, see
	// Runner.Reconfigure's doc comment) and reload.
	editConfigFile(t, path, "127.0.0.1:38080", "127.0.0.1:9999")

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=true after tray.statusAddr changed via hand-edit")
	}
}

func TestConfigSettingsReloadDetectsRestartRequiredCardRoots(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	editConfigFile(t, path, "cardRoots:\n    - \""+cfg.Ingest.CardRoots[0]+"\"", "cardRoots:\n    - \"/a-different-path\"")

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=true after ingest.cardRoots changed via hand-edit")
	}
}

func TestConfigSettingsReloadRefusesUnexpandedAPIKeyPlaceholder(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	editConfigFile(t, path, "0123456789abcdef0123456789abcdef", "${TEST_UNSET_VAR_XYZ}")

	if err := s.Reload(); err == nil {
		t.Fatal("expected Reload to refuse an unexpanded ${VAR} placeholder in server.apiKey")
	}
}
