package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// configSettings implements tray.Settings over config.Patch/config.Load
// plus a dialogRunner for the five free-text fields -- the concrete
// wiring cmd/branchdam-agent owns (internal/tray itself never imports
// internal/config or knows about zenity, matching Ingester/SelfUpdater's
// existing pattern of interfaces defined where they're consumed, not
// where they're implemented).
type configSettings struct {
	path   string
	runner *tray.Runner
	dialog dialogRunner

	// appliedStatusAddr/appliedCardRoots are what THIS PROCESS actually
	// bound/started watching at launch -- fixed for the whole process
	// lifetime, unlike s.cfg below (which reload() overwrites on every
	// call). RestartRequired is derived by diffing the current config
	// against these two, not against the previous s.cfg snapshot: a Hermes
	// review finding on this PR caught that diffing against the mutable
	// snapshot made the flag latch permanently after the first reload,
	// even if an operator reverted a hand-edit back to the original value
	// on a second reload -- the "previous" snapshot by then was already
	// the changed one, so the diff against it saw nothing.
	appliedStatusAddr string
	appliedCardRoots  []string

	mu              sync.Mutex
	cfg             config.Config
	restartRequired bool
}

// newConfigSettings builds a configSettings over the already-loaded cfg
// (runTrayCmd's own startup load -- avoids reading config.yaml twice
// before anything has changed).
func newConfigSettings(path string, cfg config.Config, runner *tray.Runner, dialog dialogRunner) *configSettings {
	return &configSettings{
		path:              path,
		cfg:               cfg,
		runner:            runner,
		dialog:            dialog,
		appliedStatusAddr: cfg.Tray.StatusAddrOrDefault(),
		appliedCardRoots:  append([]string(nil), cfg.Ingest.CardRoots...),
	}
}

func (s *configSettings) Snapshot() tray.SettingsView {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	return tray.SettingsView{
		ConfigPath:                 s.path,
		StartOnLogin:               cfg.Tray.StartOnLogin,
		SelfUpdateEnabled:          cfg.SelfUpdate.Enabled,
		SelfUpdateCheckIntervalHrs: cfg.SelfUpdate.CheckIntervalHours,
		RequireUnbuffered:          cfg.Ingest.RequireUnbuffered,
		ServerBaseURL:              cfg.Server.BaseURL,
		ServerAPIKeySet:            cfg.Server.APIKey != "",
		ArchiveRoot:                cfg.Ingest.ArchiveRoot,
		LocalEditRoot:              cfg.Ingest.LocalEditRoot,
		NamingTemplate:             cfg.Ingest.PathTemplate,
		RestartRequired:            s.restartRequired,
	}
}

func (s *configSettings) SetBool(key string, v bool) error {
	if err := s.validateBoolChange(key, v); err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	if key == "tray.startOnLogin" {
		// Best-effort, mirroring runTrayCmd's own startup registration: a
		// failure here shouldn't roll back the config change, which has
		// already been saved -- the operator's stated intent is what
		// config.yaml should reflect regardless of whether the OS
		// cooperated with actually registering the login item.
		var err error
		if v {
			err = enableStartOnLogin(s.path)
		} else {
			err = autostart.Disable()
		}
		if err != nil {
			slog.Warn("start-on-login registration change failed", "enabled", v, "err", err)
		}
	}
	return s.reload()
}

func (s *configSettings) SetInt(key string, v int) error {
	if err := s.validateIntChange(key, v); err != nil {
		return err
	}
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return s.reload()
}

// validateBoolChange/validateIntChange/validateStringChange each build a
// copy of the current config with one field hypothetically changed and
// run Validate() against it -- entirely in memory, before config.Patch
// ever touches disk. Without this, a bad value (an unexpanded ${VAR}
// placeholder typed into a dialog, a too-short API key) would be
// persisted to config.yaml by Patch and only THEN rejected by reload(),
// leaving the file and the running tray's in-memory config permanently
// diverged -- a Hermes review finding on this PR. Validating the
// hypothetical change first means an invalid value never reaches disk at
// all.
func (s *configSettings) validateBoolChange(key string, v bool) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "tray.startOnLogin":
		cfg.Tray.StartOnLogin = v
	case "selfUpdate.enabled":
		cfg.SelfUpdate.Enabled = v
	case "ingest.requireUnbuffered":
		cfg.Ingest.RequireUnbuffered = v
	}
	return firstValidateProblem(cfg)
}

func (s *configSettings) validateIntChange(key string, v int) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "selfUpdate.checkIntervalHours":
		cfg.SelfUpdate.CheckIntervalHours = v
	}
	return firstValidateProblem(cfg)
}

func (s *configSettings) validateStringChange(key, v string) error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	switch key {
	case "server.baseUrl":
		cfg.Server.BaseURL = v
	case "server.apiKey":
		cfg.Server.APIKey = v
	case "ingest.archiveRoot":
		cfg.Ingest.ArchiveRoot = v
	case "ingest.localEditRoot":
		cfg.Ingest.LocalEditRoot = v
	case "ingest.pathTemplate":
		cfg.Ingest.PathTemplate = v
	}
	return firstValidateProblem(cfg)
}

func firstValidateProblem(cfg config.Config) error {
	if problems := cfg.Validate(); len(problems) > 0 {
		return fmt.Errorf("config problem: %s", problems[0])
	}
	return nil
}

// settingsPrompt describes one PromptAndSet field's dialog.
type settingsPrompt struct {
	key     string
	kind    string // dialog.go's -kind: "entry", "password", or "directory"
	title   string
	message string
	// defaultValue pre-fills an "entry" dialog with the current value --
	// never used for "password" (a secret has no business appearing,
	// even partially, in a process's argv) or "directory".
	defaultValue func(cfg config.Config) string
}

func settingsPromptFor(field tray.SettingsField) (settingsPrompt, error) {
	switch field {
	case tray.FieldServerBaseURL:
		return settingsPrompt{
			key: "server.baseUrl", kind: "entry", title: "branchDAM Server URL",
			message:      "branchDAM server URL:",
			defaultValue: func(cfg config.Config) string { return cfg.Server.BaseURL },
		}, nil
	case tray.FieldServerAPIKey:
		return settingsPrompt{
			key: "server.apiKey", kind: "password", title: "branchDAM Agent API Key",
			message: "Agent API key (from your branchDAM server, 32+ characters):",
		}, nil
	case tray.FieldArchiveRoot:
		return settingsPrompt{
			key: "ingest.archiveRoot", kind: "directory", title: "Select the archive (NAS) folder",
		}, nil
	case tray.FieldLocalEditRoot:
		return settingsPrompt{
			key: "ingest.localEditRoot", kind: "directory", title: "Select the local edit (scratch) folder",
		}, nil
	case tray.FieldNamingTemplate:
		return settingsPrompt{
			key: "ingest.pathTemplate", kind: "entry", title: "Naming Template",
			message:      "Destination path template ({yyyy}/{mm}/{dd}/{camera_model}/{original_name}):",
			defaultValue: func(cfg config.Config) string { return cfg.Ingest.PathTemplate },
		}, nil
	default:
		return settingsPrompt{}, fmt.Errorf("settings: unknown field %v", field)
	}
}

func (s *configSettings) PromptAndSet(field tray.SettingsField) (bool, error) {
	prompt, err := settingsPromptFor(field)
	if err != nil {
		return false, err
	}

	args := []string{"-kind", prompt.kind, "-title", prompt.title}
	if prompt.message != "" {
		args = append(args, "-message", prompt.message)
	}
	if prompt.defaultValue != nil {
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		if def := prompt.defaultValue(cfg); def != "" {
			args = append(args, "-default", def)
		}
	}

	value, exitCode, err := s.dialog(args...)
	if err != nil {
		return false, fmt.Errorf("run settings dialog for %s: %w", prompt.key, err)
	}
	switch exitCode {
	case dialogExitCanceled:
		return false, nil
	case dialogExitOK:
		// fall through
	default:
		return false, fmt.Errorf("settings dialog for %s failed (exit %d)", prompt.key, exitCode)
	}

	if err := s.validateStringChange(prompt.key, value); err != nil {
		return false, err
	}
	if err := config.Patch(s.path, map[string]any{prompt.key: value}); err != nil {
		return false, fmt.Errorf("save %s: %w", prompt.key, err)
	}
	return true, s.reload()
}

// reload re-reads config.yaml, rebuilds the branchdam.Client and
// ingest.Engine it feeds, and applies them via Runner.Reconfigure.
//
// Rejects on ANY Validate() problem, not just a server.*-prefixed one:
// unlike runTrayCmd's own startup gate (which only treats server.* as
// fatal, since non-server fields are advisory-only there, matching
// preflight's WARN treatment), every field this menu can edit via dialog
// (ingest.archiveRoot/localEditRoot/pathTemplate included) is something a
// typo could hit, and this is a live config-mutation path specifically
// trying to keep bad values out -- a Hermes review finding on this PR.
// This is really a backstop for a hand-edited config.yaml reaching
// "Reload config": SetBool/SetInt/PromptAndSet already validate their
// specific change before ever calling config.Patch, so a menu-driven
// change should never reach this rejection in practice.
//
// RestartRequired is re-derived by diffing against appliedStatusAddr/
// appliedCardRoots (fixed at construction -- what THIS PROCESS actually
// has bound/running), not against the mutable previous s.cfg snapshot --
// see those fields' own doc comment for why that distinction matters.
func (s *configSettings) reload() error {
	newCfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("reload config %q: %w", s.path, err)
	}
	if problems := newCfg.Validate(); len(problems) > 0 {
		return fmt.Errorf("config problem: %s", problems[0])
	}

	client := branchdam.New(newCfg.Server.BaseURL, newCfg.Server.APIKey)
	engine := ingest.NewEngine(client, newCfg.AgentID, newCfg.Ingest, newCfg.PathMappings)

	s.mu.Lock()
	s.cfg = newCfg
	s.restartRequired = s.appliedStatusAddr != newCfg.Tray.StatusAddrOrDefault() ||
		!slices.Equal(s.appliedCardRoots, newCfg.Ingest.CardRoots)
	s.mu.Unlock()

	s.runner.Reconfigure(engine, newCfg.Ingest.CardRoots, newCfg.Ingest.LocalEditRoot)
	return nil
}

func (s *configSettings) Reload() error {
	return s.reload()
}

func (s *configSettings) OpenConfigFile() error {
	return openWithDefaultApp(s.path)
}

func (s *configSettings) RevealConfigFolder() error {
	return openWithDefaultApp(filepath.Dir(s.path))
}

// openWithDefaultApp shells out to the platform's own "open" command --
// same pattern as internal/tray/run_supported.go's openBrowser, duplicated
// rather than exported from there since this file's only reason to exist
// is wiring internal/tray.Settings, not sharing OS-shell-out helpers
// across an internal/cmd package boundary for a two-line function.
func openWithDefaultApp(path string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", path).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
