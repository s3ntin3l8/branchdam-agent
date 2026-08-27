package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// trayDialogSetup constructs runTrayCmd's dialogRunner plus the resolved
// selfExe path relaunchSelf later needs. A package-level var -- like
// preflight.go's lookPathFunc/runVersionFunc indirections -- so tests never
// re-exec the actual test binary as `dialog ...`, which would spawn `go
// test`'s own binary with nonsense flags instead of a real
// branchdam-agent. selfDialogRunner("") already fails fast and safely when
// os.Executable errs, so production code needs no separate nil check.
var trayDialogSetup = func() (run dialogRunner, selfExe string, err error) {
	selfExe, err = os.Executable()
	return selfDialogRunner(selfExe), selfExe, err
}

// runTrayCmd implements `branchdam-agent tray -config <path>`: the
// tray-resident shell around internal/ingest.Engine (issue #3). It drives
// the exact same Engine.IngestCard code path runIngestCmd (ingest.go) does
// -- neither one duplicates ingest logic, both are thin callers. On any
// platform other than windows/darwin, tray.Run returns tray.ErrUnsupported
// immediately; this is reported as a normal (non-zero-exit) error, not a
// panic, since a Linux workstation is a legitimate place to run this
// binary for its headless subcommands.
//
// Every failure path below goes through fail(), not a bare
// fmt.Fprintf(os.Stderr, ...): a `-H windowsgui`-linked tray.exe has no
// console for stderr to reach, and a macOS .app launched by launchd has
// none either -- see issue #30. fail() logs (agentlog, both stderr and the
// durable log file), and shows a best-effort error dialog naming the log
// path, before returning the exit code that failure has always used.
func runTrayCmd(args []string) int {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dialog, selfExe, exeErr := trayDialogSetup()

	logPath, closeLog, logErr := agentlog.Setup()
	defer func() { _ = closeLog() }()
	if logErr != nil {
		// Non-fatal: agentlog.Setup already fell back to an stderr-only
		// default logger. A tray that can't write its own log file should
		// still try to run rather than refuse to start entirely.
		slog.Warn("could not set up durable logging", "err", logErr)
	}
	if exeErr != nil {
		slog.Warn("could not resolve own executable path; startup-error dialogs are disabled", "err", exeErr)
	}

	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		slog.Error(msg)
		notifyStartupFailure(dialog, msg, logPath)
		return 1
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		return fail("resolve config path: %v", err)
	}

	// First-run bootstrap (issue #30): a missing config is no longer a
	// hard failure. writeStarterConfig plus a short setup wizard (server
	// URL, API key, the two ingest roots) via zenity, applied with
	// config.Patch -- see bootstrap.go. The starter config it wrote stays
	// on disk even if the wizard is canceled or a dialog fails partway,
	// so there is always something to hand-edit afterward.
	if _, statErr := os.Stat(resolvedPath); errors.Is(statErr, os.ErrNotExist) {
		slog.Info("no config found, starting first-run setup", "path", resolvedPath)
		if err := bootstrapConfigInteractive(dialog, resolvedPath); err != nil {
			if errors.Is(err, errBootstrapCanceled) {
				return fail("first-run setup canceled -- a starter config was written to %s; edit it by hand and relaunch", resolvedPath)
			}
			return fail("first-run setup failed: %v -- a starter config was written to %s; edit it by hand and relaunch", err, resolvedPath)
		}
	}

	cfg, err := config.Load(resolvedPath)
	if err != nil {
		return fail("load config %q: %v", resolvedPath, err)
	}
	// Catches the unexpanded-${VAR}-placeholder footgun (see
	// config.Validate's doc comment) for the tray the same way PR1 wired
	// it into preflight: a server.*-prefixed problem is fatal (it would
	// otherwise surface as a confusing auth failure once ingest actually
	// tries to talk to the server), anything else is advisory.
	for _, p := range cfg.Validate() {
		if strings.HasPrefix(p.Field, "server.") {
			return fail("config problem: %s", p)
		}
		slog.Warn("config problem", "field", p.Field, "message", p.Message)
	}
	if cfg.Server.APIKey == "" {
		return fail("server.apiKey is empty in config")
	}
	if cfg.Ingest.ArchiveRoot == "" || cfg.Ingest.LocalEditRoot == "" {
		return fail("ingest.archiveRoot and ingest.localEditRoot must both be set in config")
	}
	// preflight only WARNs on an empty pathMappings (an operator running
	// it hasn't necessarily configured ingest yet); the tray is about to
	// actually ingest, where a missing mapping fails downstream with a
	// confusing ErrNoPathMapping on the first real card -- fatal here.
	// This only checks non-emptiness, not that some entry actually covers
	// ingest.archiveRoot -- the wizard always writes a covering entry, but
	// a hand-authored config with an unrelated mapping still slips past
	// this to the same downstream error; hence "at least one entry" below,
	// not a coverage claim.
	if len(cfg.PathMappings) == 0 {
		return fail("pathMappings must have at least one entry -- without one covering ingest.archiveRoot, the first real card ingest will fail")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	engine := ingest.NewEngine(client, cfg.AgentID, cfg.Ingest, cfg.PathMappings)
	runner := tray.NewRunner(engine, cfg.Ingest.CardRoots, cfg.Ingest.LocalEditRoot)
	settings := newConfigSettings(resolvedPath, cfg, runner, dialog)

	var detector *ingest.Detector
	if len(cfg.Ingest.CardRoots) > 0 {
		detector = ingest.NewDetector(cfg.Ingest.CardRoots, time.Duration(cfg.Ingest.PollIntervalSecs)*time.Second)
	}

	if cfg.Tray.StartOnLogin {
		if err := enableStartOnLogin(resolvedPath); err != nil {
			// Non-fatal: an operator who opted in still gets a working
			// tray this session, just without the login item registered.
			slog.Warn("start-on-login registration failed", "err", err)
			fmt.Fprintf(os.Stderr, "branchdam-agent tray: WARN: start-on-login registration failed: %v\n", err)
		}
	}

	updater := newSelfUpdateAgent(cfg, version)

	statusSrv := tray.NewStatusServer(cfg.Tray.StatusAddrOrDefault(), func() tray.Status {
		return runner.Status(updater.Status())
	}, version)

	// Listen (not ListenAndServe) is called before tray.Run starts, so
	// the bind itself acts as this tray's single-instance guard: a
	// second tray process (including one a KeepAlive=false LaunchAgent's
	// RunAtLoad might start while a self-update relaunch is still
	// spinning up its own status server) fails here rather than silently
	// running two trays side by side.
	statusLn, err := statusSrv.Listen()
	if err != nil {
		return fail("cannot bind %s (another branchdam-agent tray may already be running): %v", statusSrv.Addr, err)
	}

	var wg sync.WaitGroup
	var statusErr, trayErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		statusErr = statusSrv.Serve(ctx, statusLn)
	}()
	go updater.Run(ctx)

	slog.Info("tray started", "statusURL", statusSrv.StatusURL())
	fmt.Printf("branchdam-agent tray: status page at %s\n", statusSrv.StatusURL())

	// selfExe was resolved at the very top (trayDialogSetup), before
	// tray.Run -- and therefore before any in-place self-update swap --
	// ever runs; relaunchSelf's own doc comment requires that ordering.
	// exeErr is only checked as fatal here, matching where this check has
	// always lived: earlier failure paths above can proceed with a
	// disabled dialogRunner (selfDialogRunner("") fails safely), but
	// nothing after this point can relaunch without a real selfExe.
	if exeErr != nil {
		return fail("resolve own executable: %v", exeErr)
	}

	var outcome tray.Outcome
	outcome, trayErr = tray.Run(ctx, runner, detector, statusSrv.StatusURL(), updater, settings)
	stop() // make sure the status server's ctx.Done() fires even if tray.Run returned on its own (e.g. Quit clicked)
	wg.Wait()

	if trayErr != nil {
		// tray.ErrUnsupported (any platform other than windows/darwin) is
		// reported the same way as any other tray error -- see this
		// function's doc comment for why that's a normal exit, not a
		// panic.
		return fail("%v", trayErr)
	}
	if statusErr != nil {
		return fail("status page: %v", statusErr)
	}

	// The successor process cannot bind statusSrv.Addr until this
	// process's listener is fully released -- wg.Wait() above already
	// guarantees Serve's graceful Shutdown completed, which is what
	// makes relaunching here (rather than from inside tray.Run's select
	// loop) safe.
	if outcome.RestartRequested {
		// AppliedVersion is empty for a settings-driven restart (issue #31:
		// tray.statusAddr or ingest.cardRoots changed, neither
		// hot-reloadable -- see Runner.Reconfigure's doc comment) as
		// opposed to a successful self-update.
		reason := "a settings change that requires a restart"
		if outcome.AppliedVersion != "" {
			reason = fmt.Sprintf("updated to %s", outcome.AppliedVersion)
		}
		slog.Info("restarting", "reason", reason)
		fmt.Printf("branchdam-agent tray: %s, restarting\n", reason)
		if err := relaunchSelf(selfExe, os.Args[1:]); err != nil {
			return fail("%s but failed to restart: %v", reason, err)
		}
	}

	return 0
}

// enableStartOnLogin registers this binary (with the same -config flag it
// was launched with) as a per-user login item. Best-effort by design --
// see internal/autostart's platform files for what "best-effort" means on
// each OS. configPath is resolved to an absolute path first: launchd and
// HKCU\...\Run both invoke the login item with an unspecified working
// directory, so a relative "-config config.yaml" (the flag's own default)
// would silently fail to find the file on next login.
func enableStartOnLogin(configPath string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path %q: %w", configPath, err)
	}
	return autostart.Enable(execPath, []string{"tray", "-config", absConfigPath})
}
