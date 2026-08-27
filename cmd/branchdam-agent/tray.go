package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// runTrayCmd implements `branchdam-agent tray -config <path>`: the
// tray-resident shell around internal/ingest.Engine (issue #3). It drives
// the exact same Engine.IngestCard code path runIngestCmd (ingest.go) does
// -- neither one duplicates ingest logic, both are thin callers. On any
// platform other than windows/darwin, tray.Run returns tray.ErrUnsupported
// immediately; this is reported as a normal (non-zero-exit) error, not a
// panic, since a Linux workstation is a legitimate place to run this
// binary for its headless subcommands.
func runTrayCmd(args []string) int {
	fs := flag.NewFlagSet("tray", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent tray: load config %q: %v\n", *configPath, err)
		return 1
	}
	if cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent tray: server.apiKey is empty in config")
		return 1
	}
	if cfg.Ingest.ArchiveRoot == "" || cfg.Ingest.LocalEditRoot == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent tray: ingest.archiveRoot and ingest.localEditRoot must both be set in config")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	engine := ingest.NewEngine(client, cfg.AgentID, cfg.Ingest, cfg.PathMappings)
	runner := tray.NewRunner(engine, cfg.Ingest.CardRoots, cfg.Ingest.LocalEditRoot)

	var detector *ingest.Detector
	if len(cfg.Ingest.CardRoots) > 0 {
		detector = ingest.NewDetector(cfg.Ingest.CardRoots, time.Duration(cfg.Ingest.PollIntervalSecs)*time.Second)
	}

	if cfg.Tray.StartOnLogin {
		if err := enableStartOnLogin(*configPath); err != nil {
			// Non-fatal: an operator who opted in still gets a working
			// tray this session, just without the login item registered.
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
		fmt.Fprintf(os.Stderr, "branchdam-agent tray: cannot bind %s (another branchdam-agent tray may already be running): %v\n", statusSrv.Addr, err)
		return 1
	}

	var wg sync.WaitGroup
	var statusErr, trayErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		statusErr = statusSrv.Serve(ctx, statusLn)
	}()
	go updater.Run(ctx)

	fmt.Printf("branchdam-agent tray: status page at %s\n", statusSrv.StatusURL())

	var outcome tray.Outcome
	outcome, trayErr = tray.Run(ctx, runner, detector, statusSrv.StatusURL(), updater)
	stop() // make sure the status server's ctx.Done() fires even if tray.Run returned on its own (e.g. Quit clicked)
	wg.Wait()

	if trayErr != nil {
		// tray.ErrUnsupported (any platform other than windows/darwin) is
		// reported the same way as any other tray error -- see this
		// function's doc comment for why that's a normal exit, not a
		// panic.
		fmt.Fprintf(os.Stderr, "branchdam-agent tray: %v\n", trayErr)
		return 1
	}
	if statusErr != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent tray: status page: %v\n", statusErr)
		return 1
	}

	// The successor process cannot bind statusSrv.Addr until this
	// process's listener is fully released -- wg.Wait() above already
	// guarantees Serve's graceful Shutdown completed, which is what
	// makes relaunching here (rather than from inside tray.Run's select
	// loop) safe.
	if outcome.RestartRequested {
		selfExe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent tray: updated to %s but could not resolve own executable to restart: %v\n", outcome.AppliedVersion, err)
			return 1
		}
		fmt.Printf("branchdam-agent tray: updated to %s, restarting\n", outcome.AppliedVersion)
		if err := relaunchSelf(selfExe, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent tray: updated to %s but failed to restart: %v\n", outcome.AppliedVersion, err)
			return 1
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
