package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/s3ntin3l8/branchdam-agent/hooks/resolve"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolvehook"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// resolveHookCandidateDirs returns the Scripts/Utility candidate list for
// the current OS, honoring an explicit scriptsDir override (from
// integrations.resolve.scriptsDir, or -dir on the resolve-hook subcommand)
// by replacing the whole candidate list rather than inserting into it --
// an operator who set this explicitly wants exactly that directory, not
// one more guess alongside the built-in ones.
func resolveHookCandidateDirs(scriptsDir string) []string {
	if scriptsDir != "" {
		return []string{scriptsDir}
	}
	home, _ := os.UserHomeDir()
	return resolvehook.CandidateDirs(runtime.GOOS, home, os.Getenv("APPDATA"))
}

// resolveHookInstallTarget picks the install destination among dirs (in
// CandidateDirs' own preference order), reinstalling in place at
// detected.Dir when the hook is ALREADY installed somewhere (e.g. after a
// version bump), and otherwise always targeting dirs[0] -- CandidateDirs'
// most-preferred, writable entry -- for a fresh install.
//
// Deliberately NOT "the first existing directory": on macOS the
// system-wide Scripts/Utility directory is typically created by the
// Resolve installer itself and so already exists even when nothing has
// been installed there, while the per-user directory CandidateDirs prefers
// often does not exist yet. Detect's own Dir field reports that first
// existing directory regardless of install state, so using it directly as
// the fresh-install target would silently pick the unwritable system path
// and fail with EACCES for an unprivileged operator -- exactly the hazard
// CandidateDirs' own ordering exists to avoid.
func resolveHookInstallTarget(dirs []string, detected resolvehook.HookState) string {
	if detected.Installed {
		return detected.Dir
	}
	return dirs[0]
}

// resolveHookInstaller implements tray.HookInstaller over
// internal/resolvehook -- the concrete wiring cmd/branchdam-agent owns,
// matching luminarSyncer's own relationship to tray.IntegrationSyncer.
type resolveHookInstaller struct {
	scriptsDir string // integrations.resolve.scriptsDir; "" means autodetect
}

func (h *resolveHookInstaller) candidateDirs() []string {
	return resolveHookCandidateDirs(h.scriptsDir)
}

// Install installs via resolveHookInstallTarget's own preference rule --
// see that function's doc comment.
func (h *resolveHookInstaller) Install(ctx context.Context) (tray.HookState, error) {
	// internal/resolvehook.Install's own atomic temp-then-rename write means
	// a mid-flight cancellation can never leave a partial file behind, so
	// this is a courtesy check rather than a hard requirement -- but
	// HookInstaller's own interface advertises ctx, and TriggerHookInstall
	// bounds every call with a menu-path timeout, so honor it before doing
	// any I/O rather than silently ignoring it.
	if err := ctx.Err(); err != nil {
		return tray.HookState{}, err
	}

	dirs := h.candidateDirs()
	if len(dirs) == 0 {
		return tray.HookState{}, fmt.Errorf("resolvehook: no candidate Scripts/Utility folder for this platform -- set integrations.resolve.scriptsDir")
	}

	detected := resolvehook.Detect(dirs, resolve.FileName, resolve.SourceSHA256)
	target := resolveHookInstallTarget(dirs, detected)

	st, err := resolvehook.Install(target, resolve.FileName, resolve.Source)
	if err != nil {
		return tray.HookState{}, err
	}
	return tray.HookState{Dir: st.Dir, Path: st.Path, Installed: st.Installed, UpToDate: st.UpToDate}, nil
}

// Reveal opens the directory the hook is (or would be) installed into --
// same preference order as Install.
func (h *resolveHookInstaller) Reveal() error {
	dirs := h.candidateDirs()
	if len(dirs) == 0 {
		return fmt.Errorf("resolvehook: no candidate Scripts/Utility folder for this platform -- set integrations.resolve.scriptsDir")
	}
	detected := resolvehook.Detect(dirs, resolve.FileName, resolve.SourceSHA256)
	dir := detected.Dir
	if dir == "" {
		dir = dirs[0]
	}
	return openWithDefaultApp(dir)
}

// runResolveHookCmd implements the headless `branchdam-agent resolve-hook`
// subcommand (issue #60) -- gives internal/resolvehook a real,
// fully-Linux-testable front door before any tray UI touches it, matching
// #46/#47's own "CLI subcommand first, matching luminar-sync" pattern. The
// tray's own resolveHookInstaller and this subcommand call the exact same
// internal/resolvehook functions -- one implementation, two front doors.
func runResolveHookCmd(args []string) int {
	fs := flag.NewFlagSet("resolve-hook", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to branchdam-agent config file (default: ./config.yaml if present, else the per-user config directory)")
	dir := fs.String("dir", "", "install directly into this Scripts/Utility directory, bypassing config/autodetection")
	install := fs.Bool("install", false, "install (or update) the hook; without this flag, only detect and report status")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	scriptsDir := *dir
	if scriptsDir == "" {
		if resolvedPath, err := config.ResolvePath(*configPath); err == nil {
			// A missing/unreadable config is not fatal here -- resolve-hook
			// falls back to autodetection just like the tray does, and an
			// operator should be able to run this before config.yaml even
			// exists (mirroring luminar-sync -dry-run's own tolerance).
			if cfg, err := config.Load(resolvedPath); err == nil {
				scriptsDir = cfg.Integrations.Resolve.ScriptsDir
			}
		}
	}

	dirs := resolveHookCandidateDirs(scriptsDir)
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "branchdam-agent resolve-hook: no candidate Scripts/Utility folder for this platform")
		return 1
	}

	if *install {
		detected := resolvehook.Detect(dirs, resolve.FileName, resolve.SourceSHA256)
		target := resolveHookInstallTarget(dirs, detected)
		st, err := resolvehook.Install(target, resolve.FileName, resolve.Source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent resolve-hook: %v\n", err)
			return 1
		}
		fmt.Printf("resolve-hook: installed at %s\n", st.Path)
		return 0
	}

	st := resolvehook.Detect(dirs, resolve.FileName, resolve.SourceSHA256)
	switch {
	case st.Dir == "":
		fmt.Println("resolve-hook: no Scripts/Utility folder found")
	case !st.Installed:
		fmt.Printf("resolve-hook: not installed (would install to %s)\n", st.Dir)
	case st.UpToDate:
		fmt.Printf("resolve-hook: installed and up to date at %s\n", st.Path)
	default:
		fmt.Printf("resolve-hook: installed but modified or out of date at %s\n", st.Path)
	}
	return 0
}
