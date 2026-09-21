package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/hooks/resolve"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolvehook"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// noopIngester satisfies tray.Ingester without touching a real card or
// server -- these tests are about configSettings' own logic (persistence,
// reload, restart-required diffing), not ingest behavior.
type noopIngester struct{}

func (noopIngester) IngestCard(_ context.Context, _ string) (ingest.CardResult, error) {
	return ingest.CardResult{}, nil
}

func (noopIngester) IngestCardOffline(_ context.Context, _ string) (ingest.OfflineCardResult, error) {
	return ingest.OfflineCardResult{}, nil
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
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"  cardRoots:\n" +
		"    - " + yamlQuote(filepath.Join(dir, "cards")) + "\n" +
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

func TestConfigSettingsStartOnLoginErrorEscapesLogLineBreaks(t *testing.T) {
	originalEnable := enableStartOnLoginFunc
	originalDisable := disableStartOnLoginFunc
	enableStartOnLoginFunc = func(string) error { return errors.New("registration failed\r\nforged entry") }
	disableStartOnLoginFunc = func() error { return errors.New("registration failed\r\nforged entry") }
	t.Cleanup(func() {
		enableStartOnLoginFunc = originalEnable
		disableStartOnLoginFunc = originalDisable
	})

	for _, tt := range []struct {
		name    string
		enabled bool
		msg     string
	}{
		{name: "enabling", enabled: true, msg: "start-on-login registration change failed while enabling"},
		{name: "disabling", enabled: false, msg: "start-on-login registration change failed while disabling"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, cfg, runner := settingsTestFixture(t)
			s := newConfigSettings(path, cfg, runner)

			var logs bytes.Buffer
			originalLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(originalLogger) })

			if err := s.SetBool("tray.startOnLogin", tt.enabled); err != nil {
				t.Fatalf("SetBool: %v", err)
			}

			var startOnLoginLine string
			for _, line := range strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n") {
				if strings.Contains(line, "start-on-login registration change failed") {
					startOnLoginLine = line
					break
				}
			}
			if startOnLoginLine == "" {
				t.Fatalf("start-on-login warning was not logged: %q", logs.String())
			}
			if !strings.Contains(startOnLoginLine, `msg="`+tt.msg+`"`) {
				t.Errorf("log did not preserve the setting: %q", startOnLoginLine)
			}
			if !strings.Contains(startOnLoginLine, `err="registration failed\r\nforged entry"`) {
				t.Errorf("log did not quote line breaks visibly: %q", startOnLoginLine)
			}
		})
	}
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
	s := newConfigSettings(path, cfg, runner)

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
	s := newConfigSettings(path, cfg, runner)

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

func TestConfigSettingsSetBoolPauseUploadOnMeteredPersistsAndReloads(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if s.Snapshot().PauseUploadOnMetered {
		t.Error("expected default PauseUploadOnMetered=false")
	}

	if err := s.SetBool("ingest.pauseUploadOnMetered", true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}

	if !s.Snapshot().PauseUploadOnMetered {
		t.Error("expected Snapshot to reflect PauseUploadOnMetered=true immediately")
	}
	if !runner.PauseUploadOnMetered() {
		t.Error("expected runner to be updated with PauseUploadOnMetered=true")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Ingest.PauseUploadOnMetered {
		t.Error("expected PauseUploadOnMetered=true to be persisted to disk")
	}
}

func TestConfigSettingsSetBoolConfirmDestructivePersistsAndReloads(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if runner.ConfirmDestructive() {
		t.Error("expected default ConfirmDestructive=false")
	}

	if err := s.SetBool("tray.confirmDestructive", true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}

	if !s.Snapshot().ConfirmDestructive {
		t.Error("expected Snapshot to reflect ConfirmDestructive=true immediately")
	}
	// Regression test for issue #211: this used to only happen via
	// settingsmenu.go's own dispatch calling SetConfirmDestructive directly
	// alongside the SetBool save -- a path the Wails Settings window (which
	// only ever calls SetBool) never had. reload() now re-seeds the live
	// Runner the same way every other live-tunable setting already does.
	if !runner.ConfirmDestructive() {
		t.Error("expected runner to be updated with ConfirmDestructive=true")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Tray.ConfirmDestructive {
		t.Error("expected ConfirmDestructive=true to be persisted to disk")
	}
}

func TestConfigSettingsSetIntPersistsAndReloads(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetInt("selfUpdate.checkIntervalHours", -1); err != nil {
		t.Fatalf("SetInt: %v", err)
	}
	if got := s.Snapshot().SelfUpdateCheckIntervalHrs; got != -1 {
		t.Errorf("SelfUpdateCheckIntervalHrs = %d, want -1", got)
	}
}

// TestConfigSettingsSetBoolRejectsUnknownKey is the regression guard for
// issue #58: validateBoolChange used to have no default case, so an
// unrecognized key silently validated an UNCHANGED cfg (reporting no
// problem) and was then written to config.yaml by config.Patch with no
// validation at all. Asserts BOTH that SetBool returns an error AND that
// config.Patch was never reached (file stays byte-for-byte unchanged) --
// the second assertion is the one that actually pins the bug, since a
// caller could return an error from some other path while still having
// already written the file.
func TestConfigSettingsSetBoolRejectsUnknownKey(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetBool("integrations.lumnar.enabled", true); err == nil {
		t.Fatal("expected SetBool to reject an unrecognized key")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("expected config.yaml to be byte-for-byte unchanged -- an unrecognized key must be rejected before config.Patch ever runs")
	}
}

func TestConfigSettingsSetIntRejectsUnknownKey(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetInt("integrations.luminar.bogusKey", 45); err == nil {
		t.Fatal("expected SetInt to reject an unrecognized key")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("expected config.yaml to be byte-for-byte unchanged for an unrecognized SetInt key")
	}
}

// TestConfigSettingsSetIntTimeoutSecs verifies that the new timeoutSecs
// key round-trips through SetInt and persists to config.yaml.
func TestConfigSettingsSetIntTimeoutSecs(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetInt("integrations.luminar.timeoutSecs", 120); err != nil {
		t.Fatalf("SetInt timeoutSecs: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Integrations.Luminar.TimeoutSecs; got != 120 {
		t.Errorf("timeoutSecs = %d, want 120", got)
	}
}

// TestConfigSettingsSetIntResolveTimeoutSecs covers the Resolve builder.
func TestConfigSettingsSetIntResolveTimeoutSecs(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetInt("integrations.resolvedb.timeoutSecs", 600); err != nil {
		t.Fatalf("SetInt timeoutSecs: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Integrations.ResolveDB.TimeoutSecs; got != 600 {
		t.Errorf("timeoutSecs = %d, want 600", got)
	}
}

// TestConfigSettingsValidateStringChangeRejectsUnknownKey exercises
// validateStringChange's default case directly -- SetString/
// SetIntegrationPath are the only current callers, and both only ever
// pass a known key, so there's no way to reach this through the public
// API today. The switch itself (config.Patch's entire allowlist) still
// deserves direct coverage independent of that.
// "integrations.lumnar.catalogPath" (note the typo) stands in for a stale
// key or a handler bug -- "integrations.luminar.catalogPath" (correctly
// spelled) is now a RECOGNIZED key as of this PR, via
// applyIntegrationStringChange.
func TestConfigSettingsValidateStringChangeRejectsUnknownKey(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.validateStringChange("integrations.lumnar.catalogPath", "/x.db"); err == nil {
		t.Fatal("expected validateStringChange to reject an unrecognized key")
	}
}

// TestConfigSettingsExistingKeysStillAccepted is a regression guard
// alongside the three tests above: adding the default: cases must not
// have narrowed the four keys that were already accepted before this PR.
func TestConfigSettingsExistingKeysStillAccepted(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetBool("tray.startOnLogin", true); err != nil {
		t.Errorf("tray.startOnLogin: %v", err)
	}
	if err := s.SetBool("selfUpdate.enabled", false); err != nil {
		t.Errorf("selfUpdate.enabled: %v", err)
	}
	if err := s.SetBool("ingest.requireUnbuffered", true); err != nil {
		t.Errorf("ingest.requireUnbuffered: %v", err)
	}
	if err := s.SetInt("selfUpdate.checkIntervalHours", 1); err != nil {
		t.Errorf("selfUpdate.checkIntervalHours: %v", err)
	}
	stringCases := map[string]string{
		"server.baseUrl":       "https://example.invalid",
		"server.apiKey":        "0123456789abcdef0123456789abcdef", // 32+ chars -- server.apiKey's own length check would otherwise reject a short value here
		"ingest.archiveRoot":   "/archive",
		"ingest.localEditRoot": "/local",
		"ingest.cardRoots":     "/media/a, /media/b",
		"ingest.pathTemplate":  "{yyyy}/{original_name}",
	}
	for key, value := range stringCases {
		if err := s.validateStringChange(key, value); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestConfigSettingsSnapshotIncludesIntegrations(t *testing.T) {
	path, _, runner := settingsTestFixture(t)
	editConfigFile(t, path, "tray:\n", "integrations:\n  nodeIndexPath: \"/data/node-index.json\"\n  luminar:\n    enabled: true\n    catalogPath: \"/data/catalog.db\"\n    dryRun: false\n    syncIntervalMinutes: 15\ntray:\n")
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, reloaded, runner)

	sv := s.Snapshot()
	if sv.NodeIndexPath != "/data/node-index.json" || !sv.NodeIndexPathSet {
		t.Errorf("NodeIndexPath = %q, NodeIndexPathSet = %v", sv.NodeIndexPath, sv.NodeIndexPathSet)
	}

	iv, ok := sv.Integration(tray.IntegrationLuminar)
	if !ok {
		t.Fatal("expected an IntegrationView for IntegrationLuminar")
	}
	if !iv.Enabled || iv.DryRun {
		t.Errorf("got Enabled=%v DryRun=%v, want Enabled=true DryRun=false", iv.Enabled, iv.DryRun)
	}
	if iv.CatalogPath != "/data/catalog.db" || !iv.CatalogPathSet {
		t.Errorf("CatalogPath = %q, CatalogPathSet = %v", iv.CatalogPath, iv.CatalogPathSet)
	}
	if iv.SyncIntervalMinutes != 15 {
		t.Errorf("SyncIntervalMinutes = %d, want 15", iv.SyncIntervalMinutes)
	}
	if iv.Title != "Luminar Neo" {
		t.Errorf("Title = %q, want %q", iv.Title, "Luminar Neo")
	}

	resolveIv, ok := sv.Integration(tray.IntegrationResolveDB)
	if !ok {
		t.Fatal("expected an IntegrationView for IntegrationResolveDB")
	}
	if resolveIv.Title != "DaVinci Resolve" {
		t.Errorf("Title = %q, want %q", resolveIv.Title, "DaVinci Resolve")
	}
}

func TestConfigSettingsSnapshotIntegrationsDefaultsUnconfigured(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	iv, ok := s.Snapshot().Integration(tray.IntegrationLuminar)
	if !ok {
		t.Fatal("expected an IntegrationView for IntegrationLuminar even when unconfigured -- one entry per registry entry, always")
	}
	if iv.Enabled || iv.CatalogPathSet {
		t.Errorf("expected a fresh config's Luminar entry to be disabled and unconfigured, got %+v", iv)
	}
	// Title is compile-time (IntegrationBuilder.Title), not read from
	// config -- it must be populated even for an integration that has
	// never been configured, so the Settings form never falls back to the
	// raw ID for a fresh install (issue #221).
	if iv.Title != "Luminar Neo" {
		t.Errorf("Title = %q, want %q even when unconfigured", iv.Title, "Luminar Neo")
	}
}

// TestConfigSettingsSetStringHappyPath drives SetString's non-interactive
// validate → patch → reload path directly, without a dialog.
func TestConfigSettingsSetStringHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("agentId", "workstation-7"); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	if got := s.Snapshot().AgentID; got != "workstation-7" {
		t.Errorf("AgentID = %q, want %q", got, "workstation-7")
	}
}

// TestConfigSettingsSetStringTrimsAgentID guards against a validate/patch
// divergence: validateStringChange trims agentId before validating, so the
// persisted value must be trimmed too, or a padded ID could validate as
// one thing and persist as another (Hermes review finding on this PR).
func TestConfigSettingsSetStringTrimsAgentID(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("agentId", "  box  "); err != nil {
		t.Fatalf("SetString: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentID != "box" {
		t.Errorf("persisted AgentID = %q, want trimmed %q", reloaded.AgentID, "box")
	}
}

// TestConfigSettingsSetStringRejectsInvalid confirms SetString runs
// validateStringChange before persisting -- an empty agentId must be
// rejected without persisting.
func TestConfigSettingsSetStringRejectsInvalid(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("agentId", "   "); err == nil {
		t.Fatal("expected an error for a blank agentId")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AgentID != "test-agent" {
		t.Errorf("AgentID = %q, want unchanged %q", reloaded.AgentID, "test-agent")
	}
}

// TestConfigSettingsSetStringRejectsPathMappings confirms pathMappings has
// no string-key path at all: issue #236 retired the lossy comma/colon
// "workstationPath:containerPath, ..." format entirely, so SetString must
// reject the key (the same way it rejects any other unrecognized string
// key) rather than silently accepting and parsing it -- and, matching
// TestConfigSettingsSetStringRejectsInvalid's own convention, the
// rejection must happen before config.Patch ever touches disk.
func TestConfigSettingsSetStringRejectsPathMappings(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("pathMappings", "/mnt/nas:/storage/archive"); err == nil {
		t.Fatal("expected SetString(pathMappings) to be rejected, got nil error")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PathMappings) != 0 {
		t.Errorf("PathMappings = %+v, want unchanged (empty)", reloaded.PathMappings)
	}
}

// TestConfigSettingsSnapshotExposesCardRoots confirms CardRoots is
// populated from cfg.Ingest.CardRoots -- SettingsView had no field for
// this at all before the structured watch-folders editor needed one to
// show the current value.
func TestConfigSettingsSnapshotExposesCardRoots(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	got := s.Snapshot().CardRoots
	if len(got) != 1 || !strings.HasSuffix(got[0], "cards") {
		t.Errorf("CardRoots = %v, want one entry ending in \"cards\" (from the fixture's cardRoots)", got)
	}
}

// TestConfigSettingsSetPathMappingsRoundTripsCommaAndDriveLetterPaths is
// the whole point of the structured route: the retired comma/colon string
// format could not represent a container path containing a comma, but
// SetPathMappings takes the fields as separate struct members, so it must.
func TestConfigSettingsSetPathMappingsRoundTripsCommaAndDriveLetterPaths(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	entries := []tray.PathMappingEntry{
		{WorkstationPath: `C:\Video, B-roll\`, ContainerPath: "/storage/b,roll/"},
	}
	if err := s.SetPathMappings(entries); err != nil {
		t.Fatalf("SetPathMappings: %v", err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PathMappings) != 1 ||
		reloaded.PathMappings[0].WorkstationPath != `C:\Video, B-roll\` ||
		reloaded.PathMappings[0].ContainerPath != "/storage/b,roll/" {
		t.Errorf("persisted pathMappings = %+v, want the comma-containing entry preserved verbatim", reloaded.PathMappings)
	}

	sv := s.Snapshot()
	if len(sv.PathMappingEntries) != 1 || sv.PathMappingEntries[0] != tray.PathMappingEntry(entries[0]) {
		t.Errorf("PathMappingEntries = %+v, want %+v", sv.PathMappingEntries, entries)
	}
}

// TestConfigSettingsSetPathMappingsEmptyWritesEmptySequence confirms
// clearing the list persists an empty YAML sequence ("[]"), not "null" --
// config.Patch's yaml.Node encoder distinguishes a nil slice from a
// non-nil empty one, and SetPathMappings must build the latter so a
// deliberate clear round-trips as "configured empty," not "unset."
func TestConfigSettingsSetPathMappingsEmptyWritesEmptySequence(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	// This fixture's ingest.uploadStream is false (the zero value), so
	// pathMappings is a required field -- SetPathMappings must still be
	// ALLOWED to write an empty list (config.Validate has no minimum-
	// length rule for it), even though the resulting configIncomplete
	// state is a separate, expected consequence checked elsewhere.
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetPathMappings(nil); err != nil {
		t.Fatalf("SetPathMappings(nil): %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "pathMappings: []") {
		t.Errorf("config.yaml does not contain \"pathMappings: []\":\n%s", raw)
	}
	if got := s.Snapshot().PathMappingEntries; len(got) != 0 {
		t.Errorf("PathMappingEntries = %v, want empty", got)
	}
}

// TestConfigSettingsSetPathMappingsRejectsEmptySide confirms an entry
// missing either side is rejected before ever reaching config.Patch.
func TestConfigSettingsSetPathMappingsRejectsEmptySide(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetPathMappings([]tray.PathMappingEntry{{WorkstationPath: "/a"}}); err == nil {
		t.Fatal("expected an error for a mapping with an empty containerPath")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PathMappings) != 0 {
		t.Errorf("pathMappings = %+v, want unchanged (empty)", reloaded.PathMappings)
	}
}

// TestConfigSettingsSetStringSliceRejectsExtensionWithoutLeadingDot
// confirms the array wire path (what the chip-list editor uses) enforces
// the same leading-dot rule the string path's splitCommaExtensions
// already does -- without this, a chip editor could persist "jpg" where
// the comma-string box would have rejected it.
func TestConfigSettingsSetStringSliceRejectsExtensionWithoutLeadingDot(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetStringSlice("ingest.allowedExtensions", []string{"jpg"}); err == nil {
		t.Fatal("expected an error for an extension without a leading dot")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Ingest.AllowedExtensions) != 0 {
		t.Errorf("allowedExtensions = %v, want unchanged (empty)", reloaded.Ingest.AllowedExtensions)
	}
}

func TestConfigSettingsSetIntegrationPathHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetIntegrationPath(tray.IntegrationLuminar, "/data/catalog.db"); err != nil {
		t.Fatalf("SetIntegrationPath: %v", err)
	}
	iv, _ := s.Snapshot().Integration(tray.IntegrationLuminar)
	if iv.CatalogPath != "/data/catalog.db" {
		t.Errorf("CatalogPath = %q", iv.CatalogPath)
	}
}

// TestConfigSettingsSetIntegrationPathResolveUsesDatabaseURLKey confirms
// SetIntegrationPath resolves the databaseUrl key (not catalogPath) for
// Resolve, the one integration where they differ, and that the Snapshot
// display value stays redacted -- ported from the deleted
// TestConfigSettingsPromptAndSetResolveDatabaseURLIsSecretSafe (issue
// #217): its dialog-argv assertions (no -default, no raw URL in argv)
// died with the dialog, but its redaction assertions guard
// databaseURLDisplay independently of any dialog and belong here.
func TestConfigSettingsSetIntegrationPathResolveUsesDatabaseURLKey(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	authValue := "fixture-value"
	databaseURL := "postgres://user:" + authValue + "@localhost:5432/resolve"
	if err := s.SetIntegrationPath(tray.IntegrationResolveDB, databaseURL); err != nil {
		t.Fatalf("SetIntegrationPath: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Integrations.ResolveDB.DatabaseURL; got != databaseURL {
		t.Errorf("databaseUrl = %q, want %q", got, databaseURL)
	}
	iv, _ := s.Snapshot().Integration(tray.IntegrationResolveDB)
	if strings.Contains(iv.CatalogPath, authValue) || iv.CatalogPath != "postgres:…" {
		t.Errorf("status display leaked or incorrectly rendered database URL: %q", iv.CatalogPath)
	}
}

func TestConfigSettingsSetIntegrationPathUnknownID(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetIntegrationPath("not-a-real-integration", "/x"); err == nil {
		t.Fatal("expected an error for an unknown integration ID")
	}
}

func TestConfigSettingsSetIntegrationRewritesHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetIntegrationRewrites(tray.IntegrationResolveDB, `D:\Videos\:/storage/archive/videos/`); err != nil {
		t.Fatalf("SetIntegrationRewrites: %v", err)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.Integrations.ResolveDB.PathRewrites
	if len(got) != 1 || got[0].From != `D:\Videos\` || got[0].To != "/storage/archive/videos/" {
		t.Errorf("pathRewrites = %v, want [{D:\\Videos\\ /storage/archive/videos/}]", got)
	}
}

func TestConfigSettingsSetIntegrationRewritesInvalid(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetIntegrationRewrites(tray.IntegrationResolveDB, "/mnt/nas/videos"); err == nil {
		t.Fatal("expected error for invalid format")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Integrations.ResolveDB.PathRewrites) != 0 {
		t.Errorf("pathRewrites = %v, want empty (should not persist on invalid input)", reloaded.Integrations.ResolveDB.PathRewrites)
	}
}

func TestConfigSettingsSetIntegrationRewritesUnsupported(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetIntegrationRewrites(tray.IntegrationLuminar, "a:b"); err == nil {
		t.Fatal("expected error for unsupported integration")
	}
}

func TestConfigSettingsReloadDetectsRestartRequiredStatusAddr(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	// Hand-edit statusAddr directly (bypassing SetBool/SetInt/SetString
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

func TestConfigSettingsReloadRejectsMissingOfflineTier0ContainerRoot(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	editConfigFile(t, path, "tray:\n", "offline:\n  queueDbPath: \""+filepath.Join(t.TempDir(), "queue.db")+"\"\n  tier0ContainerRoot: /storage\ntray:\n")
	editConfigFile(t, path, "  tier0ContainerRoot: /storage", "  tier0ContainerRoot: ")
	s := newConfigSettings(path, cfg, runner)

	if err := s.Reload(); err == nil {
		t.Fatal("expected Reload to reject an offline queue with an empty tier0ContainerRoot")
	}
}

func TestConfigSettingsReloadCardRootsDoesNotRequireRestart(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	editConfigFile(t, path, "cardRoots:\n    - "+yamlQuote(cfg.Ingest.CardRoots[0]), "cardRoots:\n    - \"/a-different-path\"")

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=false after ingest.cardRoots changed -- cardRoots is hot-reconfigurable")
	}
	if got := runner.WatchDirs(); len(got) != 1 || got[0] != "/a-different-path" {
		t.Errorf("runner.WatchDirs() = %v, want [/a-different-path]", got)
	}
}

// TestConfigSettingsSetStringCardRootsHappyPath is the non-interactive
// counterpart of the deleted TestConfigSettingsPromptAndSetCardRootsHappyPath
// (issue #217): its dialog-argv assertion died with the dialog, but the
// WatchDirs()/RestartRequired/persistence assertions guard SetString's
// own cardRoots handling and have no other coverage.
func TestConfigSettingsSetStringCardRootsHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("ingest.cardRoots", "/media/new1, /media/new2"); err != nil {
		t.Fatalf("SetString(ingest.cardRoots): %v", err)
	}
	if s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=false after SetString(ingest.cardRoots)")
	}
	if got := runner.WatchDirs(); len(got) != 2 || got[0] != "/media/new1" || got[1] != "/media/new2" {
		t.Errorf("runner.WatchDirs() = %v, want [/media/new1 /media/new2]", got)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Ingest.CardRoots) != 2 || reloaded.Ingest.CardRoots[0] != "/media/new1" || reloaded.Ingest.CardRoots[1] != "/media/new2" {
		t.Errorf("persisted cardRoots = %v, want [/media/new1 /media/new2]", reloaded.Ingest.CardRoots)
	}
}

// TestConfigSettingsSetStringCardRootsEmpty is the non-interactive
// counterpart of the deleted TestConfigSettingsPromptAndSetCardRootsEmpty
// (issue #217).
func TestConfigSettingsSetStringCardRootsEmpty(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("ingest.cardRoots", "  "); err != nil {
		t.Fatalf("SetString(ingest.cardRoots): %v", err)
	}
	if got := runner.WatchDirs(); len(got) != 0 {
		t.Errorf("expected empty WatchDirs(), got %v", got)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Ingest.CardRoots) != 0 {
		t.Errorf("expected empty persisted cardRoots, got %v", reloaded.Ingest.CardRoots)
	}
}

func TestConfigSettingsValidateStringChangeCardRootsRejectsPlaceholder(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.validateStringChange("ingest.cardRoots", "/media/good, ${UNSET_VAR_XYZ}"); err == nil {
		t.Fatal("expected validateStringChange to reject unexpanded placeholder in cardRoots")
	}
}

func TestSplitCommaPaths(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"/a, /b, /c", []string{"/a", "/b", "/c"}},
		{"  /a  ,  /b  ", []string{"/a", "/b"}},
		{"/single", []string{"/single"}},
	}
	for _, tc := range cases {
		got := splitCommaPaths(tc.input)
		if !slices.Equal(got, tc.want) {
			t.Errorf("splitCommaPaths(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestConfigSettingsReloadRefusesUnexpandedAPIKeyPlaceholder(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	editConfigFile(t, path, "0123456789abcdef0123456789abcdef", "${TEST_UNSET_VAR_XYZ}")

	if err := s.Reload(); err == nil {
		t.Fatal("expected Reload to refuse an unexpanded ${VAR} placeholder in server.apiKey")
	}
}

// TestConfigSettingsReloadRefusesNonServerPlaceholder is a regression test
// for the first Hermes-flagged bug on this PR: reload() used to only treat
// a server.*-prefixed Validate() problem as fatal (mirroring runTrayCmd's
// startup gate), silently hot-applying everything else -- including
// ingest.archiveRoot, a field this very menu edits via a folder-picker
// dialog. A hand-edit (or a dialog mistake bypassing validateStringChange
// some other way) leaving an unexpanded ${VAR} there must be rejected too.
func TestConfigSettingsReloadRefusesNonServerPlaceholder(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	editConfigFile(t, path, yamlQuote(cfg.Ingest.ArchiveRoot), yamlQuote("${TEST_UNSET_ARCHIVE_ROOT}"))

	if err := s.Reload(); err == nil {
		t.Fatal("expected Reload to refuse an unexpanded ${VAR} placeholder in ingest.archiveRoot, a non-server.* field")
	}
	if s.Snapshot().ArchiveRoot != cfg.Ingest.ArchiveRoot {
		t.Error("expected the in-memory config to keep its last-good value after a rejected reload")
	}
}

// TestConfigSettingsRestartRequiredClearsWhenReverted is a regression test
// for the second half of the restart-required diffing bug: RestartRequired
// must be re-derived from the fixed appliedStatusAddr/appliedCardRoots
// baseline on every reload, not OR-accreted against the mutable previous
// snapshot -- otherwise once true it can never go back to false, even after
// an operator reverts a hand-edit back to the value this process actually
// has bound.
func TestConfigSettingsRestartRequiredClearsWhenReverted(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	editConfigFile(t, path, "127.0.0.1:38080", "127.0.0.1:9999")
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload (changed): %v", err)
	}
	if !s.Snapshot().RestartRequired {
		t.Fatal("expected RestartRequired=true after tray.statusAddr changed via hand-edit")
	}

	editConfigFile(t, path, "127.0.0.1:9999", "127.0.0.1:38080")
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload (reverted): %v", err)
	}
	if s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=false once tray.statusAddr is reverted back to what this process actually bound")
	}
}

// TestConfigSettingsSetStringRejectsInvalidValueBeforePersisting is a
// regression test for the second Hermes-flagged bug: config.Patch used to
// run before validation, so a rejected value was still written to disk --
// this process's in-memory config and config.yaml would then silently
// diverge. validateStringChange (called from SetString before
// config.Patch) must reject a too-short API key without ever touching the
// file. Ported from the deleted
// TestConfigSettingsPromptAndSetRejectsInvalidValueBeforePersisting (issue
// #217) -- the byte-for-byte-unchanged assertion guards
// validateAndPatchString's ordering directly and has no other coverage.
func TestConfigSettingsSetStringRejectsInvalidValueBeforePersisting(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("server.apiKey", "too-short"); err == nil {
		t.Fatal("expected SetString to reject an under-32-char API key")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("expected config.yaml to be byte-for-byte unchanged when validation rejects the value before config.Patch ever runs")
	}
}

// TestConfigSettingsReloadRebuildsQueueDrainerAfterServerURLChange is a
// regression test for a Hermes review finding: queueDrainer/queuePruner
// captured *branchdam.Client by value at construction, so a
// server.baseUrl (or apiKey) change applied through the Settings menu
// left the tray's drain/prune timers silently talking to the OLD server
// forever, even though the ingest engine itself picked up the new one via
// Runner.Reconfigure. reload() must rebuild the Drainer/Pruner too,
// whenever SetQueueStore has wired a queue.db handle.
func TestConfigSettingsReloadRebuildsQueueDrainerAfterServerURLChange(t *testing.T) {
	var hitOld, hitNew int32
	handshakeOK := func(hits *int32, version string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"` + version + `","serverTimeUnix":1,"pendingEventsCount":0}`))
		}
	}
	oldSrv := httptest.NewServer(handshakeOK(&hitOld, "old"))
	defer oldSrv.Close()
	newSrv := httptest.NewServer(handshakeOK(&hitNew, "new"))
	defer newSrv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// pathMappings is required here (not just server/ingest roots) -- this
	// test is about the drainer rebuilding its client after a
	// server.baseUrl change (issue #57), and would otherwise trip the
	// finding-3 ConfigIncomplete gate on TriggerDrain added for the
	// installer's "not configured" mode.
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + oldSrv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"pathMappings:\n" +
		"  - workstationPath: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"    containerPath: \"/container/local\"\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	queueStore := openTestQueueStore(t)

	s := newConfigSettings(path, cfg, runner)
	s.SetQueueStore(queueStore)
	runner.SetQueueDeps(
		&queueCountsReader{store: queueStore},
		&queueDrainer{client: branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey), store: queueStore, agentID: cfg.AgentID},
		nil,
	)

	if _, ran := runner.TriggerDrain(context.Background()); !ran {
		t.Fatal("expected the initial TriggerDrain to run")
	}
	if atomic.LoadInt32(&hitOld) != 1 {
		t.Fatalf("expected the initial drainer to hit the old server, hitOld=%d", hitOld)
	}

	if err := s.SetString("server.baseUrl", newSrv.URL); err != nil {
		t.Fatalf("SetString(server.baseUrl): %v", err)
	}
	// reload() itself performs a handshake against the new server to sync NamingTemplate (issue #86)
	if got := atomic.LoadInt32(&hitNew); got != 1 {
		t.Fatalf("expected 1 handshake to new server during reload, got %d", got)
	}

	if _, ran := runner.TriggerDrain(context.Background()); !ran {
		t.Fatal("expected the post-reload TriggerDrain to run")
	}
	if atomic.LoadInt32(&hitNew) != 2 {
		t.Errorf("expected the rebuilt drainer to hit the new server after a server.baseUrl change, hitNew=%d -- stale-client regression", atomic.LoadInt32(&hitNew))
	}
	if atomic.LoadInt32(&hitOld) != 1 {
		t.Errorf("expected old server not to be hit again, hitOld=%d", atomic.LoadInt32(&hitOld))
	}
}

func TestConfigSettingsSetBoolRequireDCIM(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetBool("ingest.requireDCIM", true); err != nil {
		t.Fatalf("SetBool: %v", err)
	}

	if !s.Snapshot().RequireDCIM {
		t.Error("expected Snapshot to reflect RequireDCIM=true")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Ingest.RequireDCIM {
		t.Error("expected RequireDCIM=true to be persisted to disk")
	}
}

func TestConfigSettingsAllowedExtensionsValidation(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	// Valid cases
	if err := s.validateStringChange("ingest.allowedExtensions", ".arw, .cr3, .jpg"); err != nil {
		t.Errorf("expected valid extensions to pass, got %v", err)
	}
	if err := s.validateStringChange("ingest.allowedExtensions", ""); err != nil {
		t.Errorf("expected empty string to pass, got %v", err)
	}
	if err := s.validateStringChange("ingest.allowedExtensions", "  .arw ,  .dng  "); err != nil {
		t.Errorf("expected padded extensions to pass, got %v", err)
	}

	// Invalid cases (missing leading dot or bare dot)
	if err := s.validateStringChange("ingest.allowedExtensions", "arw"); err == nil {
		t.Error("expected error for extension without leading dot")
	}
	if err := s.validateStringChange("ingest.allowedExtensions", "."); err == nil {
		t.Error("expected error for bare dot extension")
	}
	if err := s.validateStringChange("ingest.allowedExtensions", ".arw, jpg"); err == nil {
		t.Error("expected error for mixed valid/invalid extensions")
	}
}

// TestConfigSettingsSetStringAllowedExtensions is the non-interactive
// counterpart of the deleted TestConfigSettingsPromptAndSetAllowedExtensions
// (issue #217): TestConfigSettingsAllowedExtensionsValidation only exercises
// validateStringChange directly, not the full SetString -> Snapshot ->
// persisted round trip with comma-separated parsing.
func TestConfigSettingsSetStringAllowedExtensions(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("ingest.allowedExtensions", ".arw, .cr3, .jpg"); err != nil {
		t.Fatalf("SetString(ingest.allowedExtensions): %v", err)
	}

	want := []string{".arw", ".cr3", ".jpg"}
	if !slices.Equal(s.Snapshot().AllowedExtensions, want) {
		t.Errorf("Snapshot AllowedExtensions = %v, want %v", s.Snapshot().AllowedExtensions, want)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reloaded.Ingest.AllowedExtensions, want) {
		t.Errorf("persisted AllowedExtensions = %v, want %v", reloaded.Ingest.AllowedExtensions, want)
	}
}

func TestConfigSettingsSetStringSliceAutoImportPaths(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	paths := []string{"/Volumes/CANON_R5", "/media/user/SONY_A7"}
	if err := s.SetStringSlice("ingest.autoImportPaths", paths); err != nil {
		t.Fatalf("SetStringSlice: %v", err)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reloaded.Ingest.AutoImportPaths, paths) {
		t.Errorf("persisted AutoImportPaths = %v, want %v", reloaded.Ingest.AutoImportPaths, paths)
	}

	// Invalid key should error
	if err := s.SetStringSlice("invalid.key", paths); err == nil {
		t.Error("expected error for invalid key")
	}
}

func TestConfigSettingsReloadSyncsNamingTemplateFromServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"serverVersion": "0.12.0",
			"serverTimeUnix": 1756470000,
			"namingTemplate": "{yyyy}/{camera_model}/{original_name}"
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"  pathTemplate: \"{original_name}\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	s := newConfigSettings(path, cfg, runner)

	// Before reload, Snapshot reflects the initial config
	if s.Snapshot().NamingTemplate != "{original_name}" {
		t.Fatalf("initial NamingTemplate = %q, want {original_name}", s.Snapshot().NamingTemplate)
	}

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// After reload, Snapshot reflects the server-synced template in memory
	want := "{yyyy}/{camera_model}/{original_name}"
	if got := s.Snapshot().NamingTemplate; got != want {
		t.Errorf("Snapshot NamingTemplate = %q, want %q", got, want)
	}

	// On disk, the config file is not modified
	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Ingest.PathTemplate != "{original_name}" {
		t.Errorf("onDisk PathTemplate = %q, want {original_name}", onDisk.Ingest.PathTemplate)
	}
}

// TestConfigSettingsReloadIgnoresPathMappingsInHandshakeResponse pins
// issue #234's fix: the server's handshake response has no pathMappings
// concept (HandshakeResponse no longer declares the field), so a Reload()
// against a config with an empty pathMappings list must leave it empty and
// keep ConfigIncomplete true -- unlike NamingTemplate, pathMappings is
// never server-populated and must be set locally (config.yaml or the
// Settings window's structured editor).
func TestConfigSettingsReloadIgnoresPathMappingsInHandshakeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"serverVersion": "0.12.0",
			"serverTimeUnix": 1756470000
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"pathMappings: []\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PathMappings) != 0 {
		t.Fatalf("expected empty pathMappings before reload, got %v", cfg.PathMappings)
	}

	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	s := newConfigSettings(path, cfg, runner)
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !runner.ConfigIncomplete() {
		t.Errorf("expected ConfigIncomplete=true since pathMappings is never server-populated, missing=%v", runner.Status(tray.UpdateStatus{}).MissingFields)
	}

	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk.PathMappings) != 0 {
		t.Errorf("expected pathMappings to remain empty on disk, got %+v", onDisk.PathMappings)
	}
}

func TestMissingRequiredFieldsByIngestMode(t *testing.T) {
	base := config.Config{
		Server:  config.ServerConfig{BaseURL: "https://branchdam.example", APIKey: "0123456789abcdef0123456789abcdef"},
		AgentID: "workstation-01",
		Ingest:  config.IngestConfig{LocalEditRoot: "/edit"},
	}

	t.Run("dual write requires archive and mapping", func(t *testing.T) {
		got := missingRequiredFields(base)
		want := []string{"ingest.archiveRoot", "pathMappings"}
		if !slices.Equal(got, want) {
			t.Fatalf("missingRequiredFields() = %v, want %v", got, want)
		}
	})

	t.Run("direct upload does not require archive or mapping", func(t *testing.T) {
		cfg := base
		cfg.Ingest.UploadStream = true
		if got := missingRequiredFields(cfg); len(got) != 0 {
			t.Fatalf("missingRequiredFields() = %v, want none", got)
		}
	})

	t.Run("direct upload with offline queue requires archive and mapping", func(t *testing.T) {
		cfg := base
		cfg.Ingest.UploadStream = true
		cfg.Offline.QueueDBPath = "/state/queue.db"
		got := missingRequiredFields(cfg)
		want := []string{"ingest.archiveRoot", "pathMappings"}
		if !slices.Equal(got, want) {
			t.Fatalf("missingRequiredFields() = %v, want %v", got, want)
		}
	})

	t.Run("agent id is required and whitespace is empty", func(t *testing.T) {
		cfg := base
		cfg.Ingest.UploadStream = true
		cfg.AgentID = "  "
		if got := missingRequiredFields(cfg); !slices.Contains(got, "agentId") {
			t.Fatalf("missingRequiredFields() = %v, want agentId", got)
		}
		if serverConfigured(cfg) {
			t.Fatal("serverConfigured() = true with an empty agentId")
		}
	})
}

func TestDirectUploadQueueOutageStopsBeforeOfflineIngestWithoutArchiveConfig(t *testing.T) {
	cfg := config.Config{
		Server:  config.ServerConfig{BaseURL: "https://branchdam.example", APIKey: "0123456789abcdef0123456789abcdef"},
		AgentID: "workstation-01",
		Ingest:  config.IngestConfig{LocalEditRoot: "/edit", UploadStream: true},
		Offline: config.OfflineConfig{QueueDBPath: "/state/queue.db"},
	}
	missing := missingRequiredFields(cfg)
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	runner.SetConfigIncomplete(len(missing) > 0, missing)
	runner.SetArchiveProber(func(context.Context, string) bool { return false })

	summary := runner.TriggerIngest(context.Background(), "/media/card")
	if !errors.Is(summary.Err, tray.ErrConfigIncomplete) {
		t.Fatalf("TriggerIngest error = %v, want ErrConfigIncomplete", summary.Err)
	}
}

func TestResolveServerConfigDoesNotHandshakeWithoutAgentID(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			BaseURL: srv.URL,
			APIKey:  "0123456789abcdef0123456789abcdef",
		},
		Ingest: config.IngestConfig{LocalEditRoot: "/edit", UploadStream: true},
	}
	_, missing := resolveServerConfig(context.Background(), &cfg, time.Second, "")
	if calls.Load() != 0 {
		t.Fatalf("handshake calls = %d, want 0", calls.Load())
	}
	if !slices.Contains(missing, "agentId") {
		t.Fatalf("missing fields = %v, want agentId", missing)
	}
}

func TestConfigSettingsReloadHandshakeUnreachablePreservesConfigTemplate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"http://127.0.0.1:1\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"  pathTemplate: \"{original_name}\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	s := newConfigSettings(path, cfg, runner)

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload should not fail when server is unreachable, got: %v", err)
	}

	if got := s.Snapshot().NamingTemplate; got != "{original_name}" {
		t.Errorf("Snapshot NamingTemplate = %q, want {original_name}", got)
	}
}

// TestConfigSettingsSetStringReSyncsNamingTemplate is the non-interactive
// counterpart of the deleted TestConfigSettingsPromptAndSetReSyncsNamingTemplate
// (issue #217): pins AGENTS.md invariant #17d -- a settings change re-runs
// the Handshake and re-syncs NamingTemplate.
func TestConfigSettingsSetStringReSyncsNamingTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok": true,
			"serverVersion": "0.12.0",
			"serverTimeUnix": 1756470000,
			"namingTemplate": "{yyyy}/{mm}/{original_name}"
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + srv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"  pathTemplate: \"{original_name}\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetString("ingest.archiveRoot", filepath.Join(dir, "new-archive")); err != nil {
		t.Fatalf("SetString(ingest.archiveRoot): %v", err)
	}

	snap := s.Snapshot()
	if snap.ArchiveRoot != filepath.Join(dir, "new-archive") {
		t.Errorf("ArchiveRoot = %q, want %q", snap.ArchiveRoot, filepath.Join(dir, "new-archive"))
	}
	if snap.NamingTemplate != "{yyyy}/{mm}/{original_name}" {
		t.Errorf("NamingTemplate = %q, want {yyyy}/{mm}/{original_name}", snap.NamingTemplate)
	}
}

// TestConfigSettingsReloadRefreshesHookStateOnScriptsDirChange pins
// issue #154 / audit F-17: when the operator edits
// integrations.resolve.scriptsDir from the Settings menu, the cached
// HookState in Runner.hookState must be re-Detected against the new
// dir before the next /status render, so the "installed and up to date"
// line reflects ground truth rather than the prior periodic-poll
// snapshot. Also: the resolveHookInstaller's own scriptsDir must be
// updated, so a subsequent TriggerHookInstall targets the new dir
// rather than the startup-captured one.
//
// Setup: pre-seed Runner.hookState with the "/old" install, hand-edit
// config.yaml to point scriptsDir at "/new" (which has no install),
// Reload, then assert Status() reports the "/new" view (Dir="",
// Installed=false).
func TestConfigSettingsReloadRefreshesHookStateOnScriptsDirChange(t *testing.T) {
	dir := t.TempDir()
	oldDir := filepath.Join(dir, "old-scripts")
	newDir := filepath.Join(dir, "new-scripts")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Hand-write the real hook file at /old so resolvehook.Detect against
	// /old reports Installed=true. The file body must match
	// resolve.SourceSHA256 for UpToDate=true; any body yields
	// Installed=true and UpToDate=false, which is also fine for this
	// test -- only the Installed/UpToDate path matters.
	if err := os.WriteFile(filepath.Join(oldDir, "branchdam_render_hook.py"), []byte("# placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "config.yaml")
	content := "" +
		"server:\n" +
		"  baseUrl: \"http://localhost:8080\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: " + yamlQuote(filepath.Join(dir, "archive")) + "\n" +
		"  localEditRoot: " + yamlQuote(filepath.Join(dir, "local")) + "\n" +
		"integrations:\n" +
		"  resolve:\n" +
		"    scriptsDir: " + yamlQuote(oldDir) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)

	// Seed Runner.hookState the same way runTrayCmd does at startup --
	// against the OLD scriptsDir, with the real Detect result.
	resolveInstaller := &resolveHookInstaller{scriptsDir: cfg.Integrations.Resolve.ScriptsDir}
	runner.SetHookInstallers(map[tray.HookID]tray.HookInstaller{tray.HookResolve: resolveInstaller})
	oldDetect := resolvehook.Detect(resolveHookCandidateDirs(cfg.Integrations.Resolve.ScriptsDir), resolve.FileName, resolve.SourceSHA256)
	runner.SetHookState(tray.HookResolve, tray.HookState{
		At:        time.Now(),
		Dir:       oldDetect.Dir,
		Path:      oldDetect.Path,
		Installed: oldDetect.Installed,
		UpToDate:  oldDetect.UpToDate,
	})

	s := newConfigSettings(path, cfg, runner)
	s.SetResolveInstaller(resolveInstaller)

	// Sanity: the seeded state reflects /old, with Installed=true.
	preStatus := runner.Status(tray.UpdateStatus{})
	preHook, ok := preStatus.Hook(tray.HookResolve)
	if !ok || preHook.State == nil {
		t.Fatal("expected initial hook state to be seeded in Status()")
	}
	if preHook.State.Dir != oldDir {
		t.Fatalf("initial Dir = %q, want %q", preHook.State.Dir, oldDir)
	}
	if !preHook.State.Installed {
		t.Fatalf("initial Installed = false, want true (we wrote the file at %s)", oldDir)
	}

	// Hand-edit the config file to point scriptsDir at /new.
	editConfigFile(t, path, "scriptsDir: "+yamlQuote(oldDir), "scriptsDir: "+yamlQuote(newDir))

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Post-reload: the installer's scriptsDir must be /new, so a later
	// TriggerHookInstall would target the right directory.
	if got := resolveInstaller.scriptsDir; got != newDir {
		t.Errorf("installer.scriptsDir = %q, want %q (reload() must update the installer on a settings change)", got, newDir)
	}
	// Post-reload: the installer's candidateDirs() must reflect /new as
	// the single override entry -- pins the single-source-of-truth
	// refactor (PR #158 re-review, cosmetic suggestion): reload() reads
	// candidateDirs() off the installer after SetScriptsDir rather than
	// computing it in parallel. A future change that adds a normalization
	// step to scriptsDir handling must land in one place, not two.
	if got := resolveInstaller.candidateDirs(); !slices.Equal(got, []string{newDir}) {
		t.Errorf("installer.candidateDirs() = %v, want [%q] (the installer's getter is the single source of truth after SetScriptsDir)", got, newDir)
	}

	// Post-reload: Runner.hookState must reflect a fresh Detect against
	// /new. /new has no install, so we expect Installed=false. Without
	// the issue #154 fix, this would still report the /old install.
	postStatus := runner.Status(tray.UpdateStatus{})
	postHook, ok := postStatus.Hook(tray.HookResolve)
	if !ok || postHook.State == nil {
		t.Fatal("expected Runner.hookState to be set after reload")
	}
	if postHook.State.Dir != "" {
		t.Errorf("post-reload Dir = %q, want \"\" -- reload() must re-Detect against %q, not surface the prior %q cached snapshot", postHook.State.Dir, newDir, oldDir)
	}
	if postHook.State.Installed {
		t.Errorf("post-reload Installed = true, want false -- the file at %s is gone from the cache's view of the world", newDir)
	}
}

func TestConfigSettingsSetBoolAutoEject(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := s.SetBool("ingest.autoEject", true); err != nil {
		t.Fatalf("SetBool ingest.autoEject: %v", err)
	}

	if !s.Snapshot().AutoEject {
		t.Error("expected Snapshot to reflect AutoEject=true")
	}

	if !runner.AutoEject() {
		t.Error("expected runner.AutoEject() to be true after reload")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Ingest.AutoEject {
		t.Error("expected ingest.autoEject to be persisted to disk")
	}
}

func TestParseResolvePathRewrites(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []config.ResolvePathRewrite
		wantErr bool
	}{
		{
			name:  "single rule",
			input: `D:\Videos\:/storage/archive/videos/`,
			want:  []config.ResolvePathRewrite{{From: `D:\Videos\`, To: "/storage/archive/videos/"}},
		},
		{
			name:  "multiple rules",
			input: `D:\Videos\:/storage/archive/videos/, F:\:/storage/archive/`,
			want: []config.ResolvePathRewrite{
				{From: `D:\Videos\`, To: "/storage/archive/videos/"},
				{From: `F:\`, To: "/storage/archive/"},
			},
		},
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:    "missing to path",
			input:   `D:\Videos\:`,
			wantErr: true,
		},
		{
			name:    "missing from path",
			input:   `:/storage`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResolvePathRewrites(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseResolvePathRewrites(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseResolvePathRewrites(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseResolvePathRewrites(%q)[%d] = %v, want %v", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFormatResolvePathRewrites(t *testing.T) {
	tests := []struct {
		name string
		in   []config.ResolvePathRewrite
		want string
	}{
		{name: "nil", in: nil, want: ""},
		{name: "single", in: []config.ResolvePathRewrite{{From: `D:\Videos`, To: "/archive"}}, want: `D:\Videos:/archive`},
		{name: "multiple", in: []config.ResolvePathRewrite{{From: `D:\`, To: "/a"}, {From: `F:\`, To: "/b"}}, want: `D:\:/a, F:\:/b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatResolvePathRewrites(tt.in); got != tt.want {
				t.Errorf("formatResolvePathRewrites = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigSettingsPair(t *testing.T) {
	var helloCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/hello" {
			helloCalled.Store(true)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"server":"branchdam","version":"1.0.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	path, cfg, runner := settingsTestFixture(t)
	// settingsTestFixture creates the file with 0644. Chmod to 0600 for pairing test.
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, cfg, runner)

	// Successful pairing
	validKey := "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	err := s.Pair(srv.URL, validKey, "paired-agent")
	if err != nil {
		t.Fatalf("Pair unexpected error: %v", err)
	}
	if !helloCalled.Load() {
		t.Fatal("expected Hello() to be called on server during pairing")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Server.BaseURL != srv.URL {
		t.Errorf("expected BaseURL %q, got %q", srv.URL, reloaded.Server.BaseURL)
	}
	if reloaded.Server.APIKey != validKey { // pragma: allowlist secret
		t.Errorf("expected APIKey to be updated, got %q", reloaded.Server.APIKey)
	}
	if reloaded.AgentID != "paired-agent" {
		t.Errorf("expected AgentID %q, got %q", "paired-agent", reloaded.AgentID)
	}

	// Server rejects key
	err = s.Pair("http://127.0.0.1:1", validKey, "some-agent")
	if err == nil {
		t.Fatal("expected error when server rejects or is unreachable")
	}

	// Refuses leaky perms
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Pair(srv.URL, validKey, "paired-agent"); err == nil {
		t.Fatal("expected error when config file has leaky perms")
	}

}
