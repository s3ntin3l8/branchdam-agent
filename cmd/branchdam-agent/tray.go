package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/hooks/resolve"
	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
	"github.com/s3ntin3l8/branchdam-agent/internal/resolvehook"
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

// trayConfirm builds the destructive-action confirmation callback the
// tray's select loop (internal/tray/run_supported.go) hands each of the
// four gated click handlers. It re-execs `dialog -kind question
// -title <title> -message <body>` via run, and maps runDialogSubprocess's
// exit-code contract to the callback's bool: dialogExitOK (operator
// clicked OK / "Yes") -> true, dialogExitCanceled (Cancel / window
// close) -> false, anything else (the dialog subprocess failed to even
// render) -> false. The last is the "refuse rather than proceed"
// default: a misconfigured host (no display, no zenity backend) is
// exactly when a destructive action should NOT fire.
//
// The ctx parameter is accepted to match tray.Run's signature. run
// bounds the subprocess itself with dialogTimeout (5 minutes), which is
// deliberately NOT the bound the tray's GUI loop waits on: the gate
// (internal/tray/gate.go) runs this callback on its own goroutine under
// a much shorter confirmTimeout and refuses the action if it hasn't
// answered by then, so a wedged dialog backend can leave this
// subprocess to expire on its own without freezing the menu.
// Logging on the failed-render path is delegated to slog (no UI side
// effect) so the menu isn't disturbed mid-click.
func trayConfirm(run dialogRunner) func(ctx context.Context, title, body string) bool {
	return func(ctx context.Context, title, body string) bool {
		// Thread ctx through to the dialog subprocess so the gate's
		// 60s confirmTimeout actually tears down a wedged dialog
		// (Hermes review on #134). Without this, the dialog was bounded
		// only by the 5-minute dialogTimeout in runDialogSubprocess,
		// not by the gate's much shorter bound.
		_, exitCode, err := run(ctx, "-kind", "question", "-title", title, "-message", body)
		if err != nil {
			slog.Warn("destructive-action confirm dialog failed to render; refusing the action", "title", title, "err", err)
			return false
		}
		switch exitCode {
		case dialogExitOK:
			return true
		case dialogExitCanceled:
			return false
		default:
			slog.Warn("destructive-action confirm dialog returned unexpected exit; refusing the action", "title", title, "exit", exitCode)
			return false
		}
	}
}

// trayIngestGate implements tray.IngestGate using dialogRunner (issue #79).
type trayIngestGate struct {
	dialog   dialogRunner
	settings *configSettings
	lookup   func(ctx context.Context, path string) (label string, size string)
}

func newTrayIngestGate(dialog dialogRunner, settings *configSettings) *trayIngestGate {
	return &trayIngestGate{
		dialog:   dialog,
		settings: settings,
		lookup:   lookupVolumeLabelAndSize,
	}
}

func (g *trayIngestGate) Confirm(ctx context.Context, volumePath, volumeName string) (bool, error) {
	cfg := g.settings.currentConfig()
	cleanPath := filepath.Clean(volumePath)
	baseName := filepath.Base(volumePath)

	// Check if volumePath or volumeName matches any entry in ingest.autoImportPaths
	for _, p := range cfg.Ingest.AutoImportPaths {
		if p == volumePath || filepath.Clean(p) == cleanPath || p == volumeName || p == baseName {
			return true, nil
		}
	}

	label, size := baseName, ""
	if g.lookup != nil {
		l, s := g.lookup(ctx, volumePath)
		if l != "" {
			label = l
		}
		size = s
	}
	if volumeName != "" && label == baseName {
		label = volumeName
	}

	display := label
	if size != "" {
		display = fmt.Sprintf("%s (%s)", label, size)
	}

	msg := fmt.Sprintf("New volume detected: %s", display)
	_, exitCode, err := g.dialog(ctx,
		"-kind", "question",
		"-title", "New volume detected",
		"-message", msg,
		"-ok-label", "Import",
		"-cancel-label", "Skip this time",
		"-extra-button", "Always auto-import",
	)
	if err != nil {
		slog.Warn("ingest confirm dialog failed to render; skipping ingest", "volume", volumePath, "err", err)
		return false, err
	}

	switch exitCode {
	case dialogExitOK:
		return true, nil
	case dialogExitExtraButton:
		// Always auto-import: write the volume label/path to ingest.autoImportPaths in config
		newPaths := append([]string(nil), cfg.Ingest.AutoImportPaths...)
		if !slices.Contains(newPaths, volumePath) {
			newPaths = append(newPaths, volumePath)
		}
		if err := g.settings.SetStringSlice("ingest.autoImportPaths", newPaths); err != nil {
			slog.Warn("failed to persist auto-import path", "path", volumePath, "err", err)
		}
		return true, nil
	case dialogExitCanceled:
		return false, nil
	default:
		slog.Warn("ingest confirm dialog returned unexpected exit; skipping ingest", "volume", volumePath, "exit", exitCode)
		return false, fmt.Errorf("dialog exit %d", exitCode)
	}
}

func lookupVolumeLabelAndSize(ctx context.Context, path string) (label string, sizeStr string) {
	label = filepath.Base(path)
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "diskutil", "info", path).Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "Volume Name:") {
					val := strings.TrimSpace(strings.TrimPrefix(line, "Volume Name:"))
					if val != "" && val != "Not applicable (no filesystem)" {
						label = val
					}
				}
				if strings.HasPrefix(line, "Total Size:") || strings.HasPrefix(line, "Volume Total Space:") {
					parts := strings.Split(line, ":")
					if len(parts) >= 2 {
						sizePart := strings.TrimSpace(parts[1])
						if idx := strings.Index(sizePart, "("); idx > 0 {
							sizeStr = strings.TrimSpace(sizePart[:idx])
						}
					}
				}
			}
		}
	case "linux":
		if lblOut, err := exec.CommandContext(ctx, "findmnt", "-n", "-o", "LABEL", path).Output(); err == nil {
			if l := strings.TrimSpace(string(lblOut)); l != "" {
				label = l
			}
		}
		if label == filepath.Base(path) {
			devPath := path
			if srcOut, err := exec.CommandContext(ctx, "findmnt", "-n", "-o", "SOURCE", path).Output(); err == nil {
				if s := strings.TrimSpace(string(srcOut)); s != "" {
					devPath = s
				}
			}
			out, err := exec.CommandContext(ctx, "lsblk", "-no", "LABEL", devPath).Output()
			if err == nil {
				if l := strings.TrimSpace(string(out)); l != "" {
					label = l
				}
			}
		}
		dfOut, err := exec.CommandContext(ctx, "df", "-Pk", path).Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(dfOut)), "\n")
			if len(lines) >= 2 {
				fields := strings.Fields(lines[1])
				if len(fields) >= 2 {
					if kbytes, err := strconv.ParseUint(fields[1], 10, 64); err == nil && kbytes > 0 {
						sizeStr = formatByteSize(kbytes * 1024)
					}
				}
			}
		}
	}
	return label, sizeStr
}

func formatByteSize(bytes uint64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := uint64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	val := float64(bytes) / float64(div)
	unitStr := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	if val >= 10 || float64(int(val)) == val {
		return fmt.Sprintf("%.0f %s", val, unitStr)
	}
	return fmt.Sprintf("%.1f %s", val, unitStr)
}

// trayPickDirectory builds the directory picker callback for the tray's
// "Import from folder…" action (issue #80). It re-execs `dialog -kind directory
// -title <title>` via run, returning the selected path on success, or an error
// if the operator dismissed the dialog (dialogExitCanceled) or if it failed to render.
func trayPickDirectory(run dialogRunner) func(ctx context.Context, title string) (string, error) {
	return func(ctx context.Context, title string) (string, error) {
		out, exitCode, err := run(ctx, "-kind", "directory", "-title", title)
		if err != nil {
			return "", err
		}
		if exitCode != dialogExitOK {
			return "", errors.New("directory picker dismissed or failed")
		}
		return strings.TrimSpace(out), nil
	}
}

// trayNotifyOS builds the OS notification callback the tray uses to show
// toast/bubble notifications (e.g. "Already ingesting this path"). It re-execs
// `dialog -kind notify -title <title> -message <message>` via run.
func trayNotifyOS(run dialogRunner) func(ctx context.Context, title, message string) {
	return func(ctx context.Context, title, message string) {
		_, _, _ = run(ctx, "-kind", "notify", "-title", title, "-message", message)
	}
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
		if p.Advisory() {
			slog.Warn("config problem", "field", p.Field, "message", p.Message)
			continue
		}
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

	// Synchronize naming template from server handshake if available (issue #86).
	// Handshake failure must not block tray startup -- continue with config-file template.
	hsCtx, hsCancel := context.WithTimeout(ctx, 5*time.Second)
	if hs, err := client.Handshake(hsCtx, branchdam.HandshakeRequest{AgentID: cfg.AgentID}); err != nil {
		slog.Warn("could not sync naming template from server handshake; using config value", "err", err)
	} else if hs.NamingTemplate != "" {
		cfg.Ingest.PathTemplate = hs.NamingTemplate
	}
	hsCancel()

	engine := ingest.NewEngine(client, cfg.AgentID, cfg.Ingest, cfg.PathMappings)
	runner := tray.NewRunner(engine, cfg.Ingest.CardRoots, cfg.Ingest.LocalEditRoot)
	runner.SetArchiveRoot(cfg.Ingest.ArchiveRoot)
	runner.SetArchiveProber(func(pctx context.Context, root string) bool {
		return probeArchive(pctx, root, cfg.Server.BaseURL, cfg.Ingest.UploadStream)
	})
	runner.SetErrorNotifier(func(title, message string) {
		if dialog != nil {
			_, _, _ = dialog(context.Background(), "-kind", "error", "-title", title, "-message", message)
		}
	})
	settings := newConfigSettings(resolvedPath, cfg, runner, dialog)

	// Integration syncers (issue #57): started unconditionally, unlike the
	// conditional prune timer below -- an integration can be enabled from
	// a later PR's Settings menu at any time the tray is running, which is
	// the entire point of startPeriodicVar re-reading its interval live
	// via settings.currentConfig() rather than a value captured once here.
	// TriggerSync's own ran=false for an unregistered/disabled ID is what
	// makes a check tick with nothing configured a free no-op.
	runner.SetIntegrationSyncers(buildIntegrationDeps(cfg, client))
	for _, b := range integrationBuilders {
		id, interval := b.ID, b.Interval
		go startPeriodicVar(ctx, integrationSyncCheckInterval, func() time.Duration {
			return interval(settings.currentConfig())
		}, integrationSyncTimeout, func(pctx context.Context) {
			runner.TriggerSync(pctx, id)
		})
	}

	// Resolve render-hook installer (issue #60): registered unconditionally,
	// same rationale as the integration syncers above -- the tray never
	// gates registration on whether the hook is already installed. Unlike a
	// sync pass, hook state is never recomputed on a timer -- see
	// HookState's own doc comment for why a live Detect on every refresh
	// tick would reproduce the statusQueueReadTimeout hazard. Detect runs
	// exactly once here, at startup; the only other place it runs again is
	// inside TriggerHookInstall, after a successful install.
	resolveInstaller := &resolveHookInstaller{scriptsDir: cfg.Integrations.Resolve.ScriptsDir}
	runner.SetHookInstallers(map[tray.HookID]tray.HookInstaller{tray.HookResolve: resolveInstaller})
	initialHookState := resolvehook.Detect(resolveHookCandidateDirs(cfg.Integrations.Resolve.ScriptsDir), resolve.FileName, resolve.SourceSHA256)
	runner.SetHookState(tray.HookResolve, tray.HookState{
		At:        time.Now(),
		Dir:       initialHookState.Dir,
		Path:      initialHookState.Path,
		Installed: initialHookState.Installed,
		UpToDate:  initialHookState.UpToDate,
	})

	// The offline queue (issue #32) is opt-in the same way `queue-drain`/
	// `prune` already are: a nil QueueReader/Drainer/Pruner is Runner's
	// honest "not configured" signal (see tray.QueueStatus's doc comment),
	// never an error. Opening queue.db here means the tray -- not
	// `queue-drain -watch` -- is now the single long-running process that
	// writes it; see docs/offline-queue.md's single-writer note.
	var queueStore *queue.Store
	if cfg.Offline.QueueDBPath != "" {
		var qErr error
		queueStore, qErr = queue.Open(cfg.Offline.QueueDBPath)
		if qErr != nil {
			slog.Warn("could not open offline queue db -- queue status and drain/prune timers are disabled this session", "path", cfg.Offline.QueueDBPath, "err", qErr)
			queueStore = nil
		} else {
			defer func() { _ = queueStore.Close() }()
			engine.Queue = queueStore
			engine.Tier0ContainerRoot = cfg.Offline.Tier0ContainerRoot
			settings.SetQueueStore(queueStore)

			var drainer tray.Drainer = &queueDrainer{client: client, store: queueStore, agentID: cfg.AgentID}
			var pruner tray.Pruner
			if cfg.Prune.Enabled {
				pruner = &queuePruner{client: client, store: queueStore, cfg: cfg}
			}
			runner.SetQueueDeps(&queueCountsReader{store: queueStore}, drainer, pruner)

			// Two independent background loops, mirroring how
			// selfUpdateAgent.Run is already started below (go
			// updater.Run(ctx)) -- not wired into
			// internal/tray/run_supported.go's menu-refresh select loop at
			// all. TriggerDrain/TriggerPrune's own locking (drainMu /
			// Runner.gate via TryLockIdle) is what keeps a timer tick and
			// a menu click from ever racing each other into a double pass.
			drainInterval := time.Duration(cfg.Offline.DrainIntervalSecsOrDefault()) * time.Second
			go startPeriodic(ctx, drainInterval, periodicPassTimeout, func(pctx context.Context) {
				runner.TriggerDrain(pctx)
			})
			if pruner != nil {
				pruneInterval := time.Duration(cfg.Prune.IntervalMinutesOrDefault()) * time.Minute
				go startPeriodic(ctx, pruneInterval, periodicPassTimeout, func(pctx context.Context) {
					runner.TriggerPrune(pctx)
				})
			}
		}
	}

	runner.SetDetectorInterval(time.Duration(cfg.Ingest.PollIntervalSecs) * time.Second)
	runner.SetDetectorRequireDCIM(cfg.Ingest.RequireDCIM)
	runner.SetIngestGate(newTrayIngestGate(dialog, settings))
	runner.SetNotifier(func(title, message string) {
		go func() {
			_, _, _ = dialog(context.Background(), "-kind", "notify", "-title", title, "-message", message)
		}()
	})

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
	}, settings.Snapshot, version)

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
	// Destructive-action confirmation gate (issue #108 / E3 #S2-14):
	// tray.confirmDestructive (default true) gates the four destructive
	// menu items behind an OK/Cancel dialog. A disabled runner (selfExe
	// couldn't be resolved earlier) yields a trayConfirm that always
	// refuses -- which matches confirmDestructive=true's "refuse on
	// anything but a clean OK" behavior, so the disabled-dialog case
	// can't accidentally turn into a silent proceed.
	confirmDestructive := cfg.Tray.ConfirmDestructive
	outcome, trayErr = tray.Run(ctx, runner, statusSrv.StatusURL(), updater, settings, trayConfirm(dialog), confirmDestructive, trayPickDirectory(dialog), trayNotifyOS(dialog))
	stop() // make sure the status server's ctx.Done() fires even if tray.Run returned on its own (e.g. Quit clicked)
	wg.Wait()

	if trayErr != nil {
		// tray.ErrUnsupported (any platform other than windows/darwin) is
		// reported the same way as any other tray error -- see this
		// function's doc comment for why that's a normal exit, not a
		// panic.
		if errors.Is(trayErr, tray.ErrUnsupported) {
			slog.Error("tray not supported on this platform", "err", trayErr)
			fmt.Fprintln(os.Stderr, trayErr)
			return 1
		}
		return fail("tray run failed: %v", trayErr)
	}
	if statusErr != nil {
		return fail("status server failed: %v", statusErr)
	}

	// The successor process cannot bind statusSrv.Addr until this
	// process's listener is fully released -- wg.Wait() above already
	// guarantees Serve's graceful Shutdown completed, which is what
	// makes relaunching here (rather than from inside tray.Run's select
	// loop) safe.
	if outcome.RestartRequested {
		// AppliedVersion is empty for a settings-driven restart (issue #31:
		// tray.statusAddr changed, not hot-reloadable -- see
		// Runner.Reconfigure's doc comment) as opposed to a successful
		// self-update or rollback (issue #33) -- RolledBack distinguishes
		// the latter two from each other.
		reason := "a settings change that requires a restart"
		switch {
		case outcome.RolledBack:
			reason = fmt.Sprintf("rolled back to %s", outcome.AppliedVersion)
		case outcome.AppliedVersion != "":
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

// probeArchive tests whether the archive destination is reachable before an
// online ingest pass is attempted. If uploadStream is true, it verifies that
// server.baseUrl/healthz answers with HTTP 200 within a 2-second timeout;
// otherwise, it verifies that archiveRoot can be stat'd within 2 seconds.
func probeArchive(ctx context.Context, archiveRoot, baseURL string, uploadStream bool) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if uploadStream {
		if baseURL == "" {
			return false
		}
		healthzURL := strings.TrimRight(baseURL, "/") + "/healthz"
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthzURL, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}

	if archiveRoot == "" {
		return false
	}

	statCh := make(chan error, 1)
	go func() {
		_, err := os.Stat(archiveRoot)
		statCh <- err
	}()

	select {
	case <-probeCtx.Done():
		return false
	case err := <-statCh:
		return err == nil
	}
}
