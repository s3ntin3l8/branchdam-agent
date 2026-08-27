package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

	mu              sync.Mutex
	cfg             config.Config
	restartRequired bool
}

// newConfigSettings builds a configSettings over the already-loaded cfg
// (runTrayCmd's own startup load -- avoids reading config.yaml twice
// before anything has changed).
func newConfigSettings(path string, cfg config.Config, runner *tray.Runner, dialog dialogRunner) *configSettings {
	return &configSettings{path: path, cfg: cfg, runner: runner, dialog: dialog}
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
	if err := config.Patch(s.path, map[string]any{key: v}); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return s.reload()
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

	if err := config.Patch(s.path, map[string]any{prompt.key: value}); err != nil {
		return false, fmt.Errorf("save %s: %w", prompt.key, err)
	}
	return true, s.reload()
}

// reload re-reads config.yaml, rebuilds the branchdam.Client and
// ingest.Engine it feeds, and applies them via Runner.Reconfigure.
// RestartRequired is re-derived by diffing the two restart-only fields
// against the previous snapshot on every call -- deliberately not tracked
// per-key, so a hand-edit followed by "Reload config" is caught exactly
// the same way a menu-driven change is.
func (s *configSettings) reload() error {
	newCfg, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("reload config %q: %w", s.path, err)
	}
	for _, p := range newCfg.Validate() {
		if strings.HasPrefix(p.Field, "server.") {
			return fmt.Errorf("config problem: %s", p)
		}
	}

	client := branchdam.New(newCfg.Server.BaseURL, newCfg.Server.APIKey)
	engine := ingest.NewEngine(client, newCfg.AgentID, newCfg.Ingest, newCfg.PathMappings)

	s.mu.Lock()
	oldCfg := s.cfg
	s.cfg = newCfg
	s.restartRequired = s.restartRequired ||
		oldCfg.Tray.StatusAddrOrDefault() != newCfg.Tray.StatusAddrOrDefault() ||
		!slices.Equal(oldCfg.Ingest.CardRoots, newCfg.Ingest.CardRoots)
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
