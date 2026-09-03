package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
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
	s := newConfigSettings(path, cfg, runner, nil)

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
	s := newConfigSettings(path, cfg, runner, nil)

	if err := s.SetInt("integrations.luminar.timeoutSecs", 45); err == nil {
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

// TestConfigSettingsValidateStringChangeRejectsUnknownKey exercises
// validateStringChange's default case directly -- PromptAndSet/
// PromptAndSetIntegrationPath are the only current callers, and both only
// ever pass a known key (settingsPromptFor's own keys, or
// integrationBuilders-derived ones), so there's no way to reach this
// through the public API today. The switch itself (config.Patch's entire
// allowlist) still deserves direct coverage independent of that.
// "integrations.lumnar.catalogPath" (note the typo) stands in for a stale
// key or a handler bug -- "integrations.luminar.catalogPath" (correctly
// spelled) is now a RECOGNIZED key as of this PR, via
// applyIntegrationStringChange.
func TestConfigSettingsValidateStringChangeRejectsUnknownKey(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	if err := s.validateStringChange("integrations.lumnar.catalogPath", "/x.db"); err == nil {
		t.Fatal("expected validateStringChange to reject an unrecognized key")
	}
}

// TestConfigSettingsExistingKeysStillAccepted is a regression guard
// alongside the three tests above: adding the default: cases must not
// have narrowed the four keys that were already accepted before this PR.
func TestConfigSettingsExistingKeysStillAccepted(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

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

func TestConfigSettingsPromptAndSetHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	var gotArgs []string
	dialog := func(_ context.Context, args ...string) (string, int, error) {
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
	dialog := func(_ context.Context, args ...string) (string, int, error) {
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
	dialog := func(_ context.Context, args ...string) (string, int, error) {
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
	dialog := func(_ context.Context, args ...string) (string, int, error) {
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

func TestConfigSettingsSnapshotIncludesIntegrations(t *testing.T) {
	path, _, runner := settingsTestFixture(t)
	editConfigFile(t, path, "tray:\n", "integrations:\n  nodeIndexPath: \"/data/node-index.json\"\n  luminar:\n    enabled: true\n    catalogPath: \"/data/catalog.db\"\n    dryRun: false\n    syncIntervalMinutes: 15\ntray:\n")
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newConfigSettings(path, reloaded, runner, nil)

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
}

func TestConfigSettingsSnapshotIntegrationsDefaultsUnconfigured(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	iv, ok := s.Snapshot().Integration(tray.IntegrationLuminar)
	if !ok {
		t.Fatal("expected an IntegrationView for IntegrationLuminar even when unconfigured -- one entry per registry entry, always")
	}
	if iv.Enabled || iv.CatalogPathSet {
		t.Errorf("expected a fresh config's Luminar entry to be disabled and unconfigured, got %+v", iv)
	}
}

func TestConfigSettingsPromptAndSetIntegrationPathHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	var gotArgs []string
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		gotArgs = args
		return "/data/catalog.db", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSetIntegrationPath(tray.IntegrationLuminar)
	if err != nil {
		t.Fatalf("PromptAndSetIntegrationPath: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	iv, _ := s.Snapshot().Integration(tray.IntegrationLuminar)
	if iv.CatalogPath != "/data/catalog.db" {
		t.Errorf("CatalogPath = %q", iv.CatalogPath)
	}
	if !slices.Contains(gotArgs, "file") {
		t.Errorf("expected -kind file in dialog args, got %v", gotArgs)
	}
	if !slices.Contains(gotArgs, "-patterns") {
		t.Errorf("expected -patterns in dialog args (Luminar's CatalogFilePatterns), got %v", gotArgs)
	}
}

func TestConfigSettingsPromptAndSetIntegrationPathCanceled(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		return "", dialogExitCanceled, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSetIntegrationPath(tray.IntegrationLuminar)
	if err != nil {
		t.Fatalf("expected no error on cancel, got %v", err)
	}
	if ok {
		t.Error("expected ok=false on cancel")
	}
	iv, _ := s.Snapshot().Integration(tray.IntegrationLuminar)
	if iv.CatalogPathSet {
		t.Error("expected CatalogPath unchanged after a canceled prompt")
	}
}

func TestConfigSettingsPromptAndSetIntegrationPathUnknownID(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	if _, err := s.PromptAndSetIntegrationPath("not-a-real-integration"); err == nil {
		t.Fatal("expected an error for an unknown integration ID")
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

func TestConfigSettingsReloadCardRootsDoesNotRequireRestart(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

	editConfigFile(t, path, "cardRoots:\n    - \""+cfg.Ingest.CardRoots[0]+"\"", "cardRoots:\n    - \"/a-different-path\"")

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

func TestConfigSettingsPromptAndSetCardRootsHappyPath(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	var gotArgs []string
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		gotArgs = args
		return "/media/new1, /media/new2", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldCardRoots)
	if err != nil {
		t.Fatalf("PromptAndSet(FieldCardRoots): %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if s.Snapshot().RestartRequired {
		t.Error("expected RestartRequired=false after PromptAndSet(FieldCardRoots)")
	}
	if got := runner.WatchDirs(); len(got) != 2 || got[0] != "/media/new1" || got[1] != "/media/new2" {
		t.Errorf("runner.WatchDirs() = %v, want [/media/new1 /media/new2]", got)
	}
	if !slices.Contains(gotArgs, "entry") {
		t.Errorf("expected -kind entry in dialog args, got %v", gotArgs)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Ingest.CardRoots) != 2 || reloaded.Ingest.CardRoots[0] != "/media/new1" || reloaded.Ingest.CardRoots[1] != "/media/new2" {
		t.Errorf("persisted cardRoots = %v, want [/media/new1 /media/new2]", reloaded.Ingest.CardRoots)
	}
}

func TestConfigSettingsPromptAndSetCardRootsEmpty(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		return "  ", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldCardRoots)
	if err != nil {
		t.Fatalf("PromptAndSet(FieldCardRoots): %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
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
	s := newConfigSettings(path, cfg, runner, nil)

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
	s := newConfigSettings(path, cfg, runner, nil)

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
	s := newConfigSettings(path, cfg, runner, nil)

	editConfigFile(t, path, cfg.Ingest.ArchiveRoot, "${TEST_UNSET_ARCHIVE_ROOT}")

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
	s := newConfigSettings(path, cfg, runner, nil)

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

// TestConfigSettingsPromptAndSetRejectsInvalidValueBeforePersisting is a
// regression test for the second Hermes-flagged bug: config.Patch used to
// run before validation, so a rejected value was still written to disk --
// this process's in-memory config and config.yaml would then silently
// diverge. validateStringChange (called from PromptAndSet before
// config.Patch) must reject a too-short API key without ever touching the
// file.
func TestConfigSettingsPromptAndSetRejectsInvalidValueBeforePersisting(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		return "too-short", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldServerAPIKey)
	if err == nil {
		t.Fatal("expected PromptAndSet to reject an under-32-char API key")
	}
	if ok {
		t.Error("expected ok=false when validation rejects the value")
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
	content := "" +
		"server:\n" +
		"  baseUrl: \"" + oldSrv.URL + "\"\n" +
		"  apiKey: \"0123456789abcdef0123456789abcdef\"\n" +
		"agentId: \"test-agent\"\n" +
		"ingest:\n" +
		"  archiveRoot: \"" + filepath.Join(dir, "archive") + "\"\n" +
		"  localEditRoot: \"" + filepath.Join(dir, "local") + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := tray.NewRunner(noopIngester{}, nil, cfg.Ingest.LocalEditRoot)
	queueStore := openTestQueueStore(t)

	dialog := func(_ context.Context, args ...string) (string, int, error) {
		return newSrv.URL, dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)
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

	if _, err := s.PromptAndSet(tray.FieldServerBaseURL); err != nil {
		t.Fatalf("PromptAndSet: %v", err)
	}

	if _, ran := runner.TriggerDrain(context.Background()); !ran {
		t.Fatal("expected the post-reload TriggerDrain to run")
	}
	if atomic.LoadInt32(&hitNew) != 1 {
		t.Errorf("expected the rebuilt drainer to hit the new server after a server.baseUrl change, hitNew=%d -- stale-client regression", hitNew)
	}
}

func TestConfigSettingsSetBoolRequireDCIM(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner, nil)

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
	s := newConfigSettings(path, cfg, runner, nil)

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

func TestConfigSettingsPromptAndSetAllowedExtensions(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	dialog := func(_ context.Context, args ...string) (string, int, error) {
		return ".arw, .cr3, .jpg", dialogExitOK, nil
	}
	s := newConfigSettings(path, cfg, runner, dialog)

	ok, err := s.PromptAndSet(tray.FieldAllowedExtensions)
	if err != nil {
		t.Fatalf("PromptAndSet: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
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
