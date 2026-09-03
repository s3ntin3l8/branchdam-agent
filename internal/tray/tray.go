// Package tray is the tray-resident shell around internal/ingest.Engine
// (issue #3, M1's tray half). Per the plan doc's "UI stack (M1)" section
// and this repo's CLAUDE.md ingest-core comment, the ingest core has no UI
// imports on purpose -- this package is a thin driver over it, not a
// reimplementation. Everything in this file is platform-independent (no
// fyne.io/systray import): the actual tray icon/menu wiring lives in
// run_supported.go (windows/darwin only, see that file's doc comment),
// kept separate so the state this package tracks (watch directories, the
// M2-stub queue status, the last ingest outcome) is unit-testable on any
// host, including the Linux CI runner that builds and tests everything
// else in this repo.
package tray

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// ErrUnsupported is returned by Run on any platform other than
// windows/darwin. The tray is scoped to those two per the plan doc and
// issue #3; a Linux workstation still has the fully-tested headless
// `ingest`/`ingest --watch` path, just no tray icon.
var ErrUnsupported = errors.New("tray: unsupported on this platform (windows and darwin only); use `branchdam-agent ingest` instead")

// Ingester is the subset of *ingest.Engine's surface Runner needs, so tests
// can substitute a fake without touching a real card or a real branchDAM
// server -- same pattern as internal/ingest.Engine's own nodeCreator
// interface and cmd/branchdam-agent/preflight.go's helloCaller.
type Ingester interface {
	IngestCard(ctx context.Context, cardRoot string) (ingest.CardResult, error)
	IngestCardOffline(ctx context.Context, cardRoot string) (ingest.OfflineCardResult, error)
}

// IngestSummary is a condensed, human-readable view of the most recent
// IngestCard call -- what the status page and the tray tooltip both show.
type IngestSummary struct {
	CardPath  string
	StartedAt time.Time
	Elapsed   time.Duration
	Submitted int
	Skipped   int
	Failed    int
	Err       error
	Offline   bool
}

// OK reports whether every file that should have been submitted succeeded
// (a deliberate sidecar skip does not count as a failure) -- mirrors
// cmd/branchdam-agent/ingest.go's printIngestReport logic exactly, since
// the tray drives the same underlying Engine and should reach the same
// verdict from the same CardResult shape.
func (s IngestSummary) OK() bool {
	return s.Err == nil && s.Failed == 0
}

// Outcome is why Run returned -- specifically, whether the operator asked
// for an update to be installed or a previous one rolled back. Untagged
// (unlike Run itself) so both run_supported.go and run_unsupported.go's
// stub can share the type.
type Outcome struct {
	RestartRequested bool
	AppliedVersion   string
	// RolledBack distinguishes a successful "Roll back to vX" from a
	// forward self-update -- both set AppliedVersion (the version now
	// running after the restart), but the caller's log line should say
	// "rolled back to" rather than "updated to".
	RolledBack bool
}

// SelfUpdater is the subset of the self-update subsystem the tray drives:
// an interface, like Ingester, so this package's only dependency stays
// internal/ingest and it remains unit-testable on any host with no
// network and no go-selfupdate import -- internal/selfupdate pulls in
// golang.org/x/crypto/openpgp transitively (see that package's doc
// comment), which this package has no reason to carry.
type SelfUpdater interface {
	Status() UpdateStatus
	ApplyLatest(ctx context.Context) (string, error)
	// RollbackAvailable reports whether a previously applied version can
	// be restored right now, and if so, which one -- a cheap local
	// filesystem check (internal/selfupdate.HasRollback/PreviousVersion),
	// safe to call on every menu-refresh tick, never a network call.
	RollbackAvailable() (version string, ok bool)
	// Rollback restores the previously applied version. Mirrors
	// ApplyLatest's own contract: the caller is responsible for holding
	// Runner.TryLockIdle for the call's entire duration.
	Rollback(ctx context.Context) (string, error)
}

// UpdateStatus is a structured snapshot of the self-update subsystem's
// state -- what used to be collapsed into a single Status.SelfUpdateNote
// string. Structured so the tray menu can show/hide and enable/disable
// "Install and restart" on UpdateFound directly, rather than parsing
// prose.
type UpdateStatus struct {
	Enabled        bool
	Checked        bool
	CheckedAt      time.Time
	CurrentVersion string
	LatestVersion  string
	UpdateFound    bool
	// Unavailable is set once the running version is not semver (e.g. a
	// locally built "dev" binary) -- self-update is structurally
	// impossible for such a build, so callers should stop re-checking.
	Unavailable bool
	Err         error
	// Applied is set to the version string once ApplyLatest has
	// succeeded this session.
	Applied string
}

// Note renders UpdateStatus as the one-line string the status page shows
// -- the same text startSelfUpdateCheck used to compute directly before
// this type existed.
func (u UpdateStatus) Note() string {
	switch {
	case !u.Enabled:
		return "disabled (selfUpdate.enabled: false in config)"
	case u.Applied != "":
		return fmt.Sprintf("updated to %s -- restarting", u.Applied)
	case u.Unavailable:
		return "unavailable (not a released build)"
	case u.Err != nil:
		return fmt.Sprintf("check failed: %v", u.Err)
	case !u.Checked:
		return "checking..."
	case u.UpdateFound:
		return fmt.Sprintf("update available: %s -> %s", u.CurrentVersion, u.LatestVersion)
	default:
		return fmt.Sprintf("up to date (%s)", u.CurrentVersion)
	}
}

// Status is the full snapshot the embedded status page and the tray menu
// both render from. QueueStatus replaced the M2-era QueueStatusStub in
// issue #32 -- see that type's own doc comment for the
// Configured-vs-Err distinction that preserves the "never fabricate a
// number" invariant the stub used to hold as a literal string.
type Status struct {
	WatchDirs   []string
	ScratchNote string
	QueueStatus QueueStatus
	LastIngest  *IngestSummary
	SelfUpdate  UpdateStatus
	// Busy and BusyCard reflect Runner.Busy() -- shown on the status page
	// and used by the tray menu to disable "Install and restart" while an
	// ingest is running (see Runner.TryLockIdle for the actual gate this
	// only mirrors for display).
	Busy     bool
	BusyCard string
	// BusySince is when the current ingest started (zero when not busy).
	BusySince time.Time
	// HandshakeOK surfaces the most recent DrainSummary.HandshakeOK to the
	// status page -- the operator's most basic "is the server reachable?"
	// signal, which DrainSummary already computes but never exposed. False
	// when no drain has run yet (see HasDrained for that distinction);
	// this is the single source of truth for the "Server connection"
	// section's reachable/unreachable label, so the template does not
	// reach into QueueStatus.LastDrain directly.
	HandshakeOK bool
	// HasDrained is true once at least one drain pass has completed in
	// this session -- the "have we ever heard from the server?" signal
	// the Server connection section uses to distinguish "no drain run
	// yet" from "last drain: handshake failed". Independent of
	// HandshakeOK on purpose: a never-drained install is neither
	// "reachable" nor "unreachable", it's "unknown".
	HasDrained bool
	// InFlightDrain reports whether a drain pass is currently running.
	InFlightDrain bool
	// InFlightPrune reports whether a prune pass is currently running.
	InFlightPrune bool
	// Integrations is RUNTIME state only, ordered by the compile-time
	// Integrations() registry so the status page and (a later PR's) menu
	// render in the same order every time. Config state (enabled, dry
	// run, configured paths) comes from Settings.Snapshot(), never from
	// here -- Runner never reads config.
	Integrations []IntegrationStatus
	// Hooks is Integrations' counterpart for installable script hooks
	// (DaVinci Resolve's render hook, issue #60) -- a SEPARATE list, not
	// folded into Integrations, since a hook has no CatalogSyncConfig and
	// isn't in Integrations()'s own registry (see HookID's own doc
	// comment). Ordered by the compile-time Hooks() registry.
	Hooks []HookStatus

	// LastIngestWasOffline reports whether the most recent ingest took the
	// offline fallback path.
	LastIngestWasOffline bool
	// PendingOfflineCount is the number of pending items in the offline
	// queue (convenience for tooltip and status display).
	PendingOfflineCount int
}

// Runner owns the state a tray-resident process needs: the ingest engine
// to drive, which directories to describe as "watched," and the outcome of
// the most recent ingest run. It has no UI imports -- run_supported.go
// wraps one in the actual systray.Run(...) call.
type Runner struct {
	// gate is held for the ENTIRE duration of an IngestCard call. It
	// serializes the tray's two ingest paths (a menu click and the
	// card-detection watch loop) -- nothing did before this field
	// existed, and two concurrent IngestCard runs over the same card
	// root race internal/ingest's destination-collision resolution and
	// can mint spurious "_2" copies, a real bug independent of
	// self-update. It also doubles as the self-update gate: TryLockIdle
	// below acquires this same mutex without blocking, so "Install and
	// restart" can refuse outright while a card is mid-copy instead of
	// queuing behind it. Reconfigure holds it too, for the same reason:
	// ingester/watchDirs/scratchDir must never change out from under a
	// card mid-copy.
	gate sync.Mutex

	// drainMu is a SEPARATE mutex from gate, deliberately -- see
	// TriggerDrain's doc comment for why a drain pass must never be
	// blocked by (or block) an ingest or a self-update apply.
	drainMu sync.Mutex

	// mu guards every field below, including ingester/watchDirs/
	// scratchDir -- unlike before issue #31's settings menu, these are no
	// longer set once at construction and left alone; Reconfigure can
	// swap them at any time the tray is running.
	mu         sync.Mutex
	ingester   Ingester
	watchDirs  []string
	scratchDir string // LocalEditRoot -- described, not measured; see Status().
	last       *IngestSummary
	busy       bool
	busyCard   string
	busySince  time.Time

	// queueReader/drainer/pruner are nil-able (issue #32): nil means "not
	// configured" (no offline.queueDbPath, or prune.enabled is false),
	// the honest signal QueueStatus.Configured/PruneEnabled surface --
	// never treated as an error. Set once via SetQueueDeps, not swapped by
	// Reconfigure: unlike ingester/watchDirs/scratchDir, queue.db is
	// opened once at tray startup (cmd/branchdam-agent/tray.go) and its
	// path is not one of the fields issue #31's settings menu can edit, so
	// there is nothing here that needs to survive a live config reload.
	queueReader   QueueReader
	drainer       Drainer
	pruner        Pruner
	lastDrain     *DrainSummary
	lastPrune     *PruneSummary
	inFlightDrain bool
	inFlightPrune bool

	// syncers is nil-able per ID (issue #57), same contract as drainer/
	// pruner above: an absent entry is "not configured" -- disabled, or
	// enabled but missing a required config field -- never an error. Set
	// via SetIntegrationSyncers, which (unlike queueReader/drainer/pruner)
	// IS re-called on every settings reload, since an integration's
	// enabled/dry-run/configured state can change at any time the tray is
	// running, not just at startup.
	syncers map[IntegrationID]IntegrationSyncer
	// syncInFlight tracks in-progress Sync calls PER ID, deliberately not
	// a single shared mutex like drainMu: two different integrations (e.g.
	// luminar and a future lrcat) must never block each other, only
	// concurrent passes for the SAME id. See TriggerSync's own doc
	// comment for why this shares neither gate nor drainMu.
	syncInFlight map[IntegrationID]bool
	lastSync     map[IntegrationID]*SyncSummary

	// hookInstallers/hookInFlight/hookState mirror syncers/syncInFlight/
	// lastSync's own shape exactly (issue #60), one map keyed by HookID
	// instead of IntegrationID -- a hook is a separate concept (see
	// HookID's own doc comment), not a slot in the integrations maps
	// above. hookState is populated via SetHookState at tray startup and
	// updated by TriggerHookInstall on completion -- NEVER computed live
	// from Status() or a refresh tick; see SetHookState's own doc
	// comment for why.
	hookInstallers map[HookID]HookInstaller
	hookInFlight   map[HookID]bool
	hookState      map[HookID]*HookState

	// archiveRoot is the configured ingest.archiveRoot destination path,
	// probed before an online ingest pass.
	archiveRoot string
	// archiveProber is the probe function called by TriggerIngest to verify
	// reachability before attempting an online dual-write ingest pass.
	archiveProber func(ctx context.Context, archiveRoot string) bool
	// notifyError is called when an ingest fails (e.g. NAS unreachable with
	// no offline queue configured).
	notifyError func(title, message string)

	// detectorMu guards detectorCancel, detectorDone, and detectorBaseCtx
	// (issue #78) across ReconfigureDetector / Reconfigure / StopDetector calls.
	detectorMu          sync.Mutex
	detectorCancel      context.CancelFunc
	detectorDone        chan struct{}
	detectorBaseCtx     context.Context
	detectorInterval    time.Duration
	detectorRequireDCIM bool
	onCardIngested      func()
	detectorErrHandler  func(err error)
}

// NewRunner builds a Runner over ingester, describing watchDirs (typically
// the configured ingest.cardRoots) and scratchDir (ingest.localEditRoot)
// in the status snapshot.
func NewRunner(ingester Ingester, watchDirs []string, scratchDir string) *Runner {
	return &Runner{
		ingester:         ingester,
		watchDirs:        append([]string(nil), watchDirs...),
		scratchDir:       scratchDir,
		detectorInterval: ingest.DefaultPollInterval,
		syncInFlight:     map[IntegrationID]bool{},
		lastSync:         map[IntegrationID]*SyncSummary{},
		hookInFlight:     map[HookID]bool{},
		hookState:        map[HookID]*HookState{},
	}
}

// SetArchiveRoot updates the archive destination directory (ingest.archiveRoot)
// probed before an online ingest.
func (r *Runner) SetArchiveRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.archiveRoot = root
}

// SetArchiveProber sets a custom reachability prober called before IngestCard.
// If nil, no probe is performed and IngestCard is called directly.
func (r *Runner) SetArchiveProber(prober func(ctx context.Context, archiveRoot string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.archiveProber = prober
}

// SetErrorNotifier sets a callback invoked when an ingest cannot proceed
// (e.g. archive unreachable and no offline queue configured).
func (r *Runner) SetErrorNotifier(fn func(title, message string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifyError = fn
}

// WatchDirs returns a defensive copy of the directories currently
// described as watched -- run_supported.go's menu rendering and its
// "Ingest now" worker both read this rather than a raw field, since
// Reconfigure can change it while the tray is running.
func (r *Runner) WatchDirs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.watchDirs...)
}

// TriggerIngest runs one IngestCard pass over cardPath through the same
// Engine the headless `ingest` subcommand uses, records the outcome, and
// returns it. Safe to call from a menu-click handler or from the
// card-detection watch loop -- both paths in run_supported.go go through
// this single method, and it now also serializes them: a call blocks
// until any other in-flight ingest (from either path) has finished, via
// gate.
//
// If the archive destination is probed and found unreachable:
//   - If offline queueing is configured (queueReader != nil), it transparently
//     falls back to IngestCardOffline.
//   - If offline queueing is not configured, it records an error and invokes
//     the registered error notification handler.
func (r *Runner) TriggerIngest(ctx context.Context, cardPath string) IngestSummary {
	r.gate.Lock()
	defer r.gate.Unlock()

	r.setBusy(true, cardPath)
	defer r.setBusy(false, "")

	r.mu.Lock()
	ingester := r.ingester
	prober := r.archiveProber
	archiveRoot := r.archiveRoot
	hasQueue := r.queueReader != nil
	notifyErr := r.notifyError
	r.mu.Unlock()

	summary := IngestSummary{CardPath: cardPath, StartedAt: time.Now()}

	reachable := true
	if prober != nil {
		reachable = prober(ctx, archiveRoot)
	}

	if reachable {
		result, err := ingester.IngestCard(ctx, cardPath)
		summary.Elapsed = time.Since(summary.StartedAt)
		if err != nil {
			summary.Err = err
		} else {
			for _, f := range result.Files {
				switch {
				case f.Err != nil:
					summary.Failed++
				case f.Skipped:
					summary.Skipped++
				default:
					summary.Submitted++
				}
			}
		}
	} else if hasQueue {
		summary.Offline = true
		result, err := ingester.IngestCardOffline(ctx, cardPath)
		summary.Elapsed = time.Since(summary.StartedAt)
		if err != nil {
			summary.Err = err
		} else {
			for _, f := range result.Files {
				switch {
				case f.Err != nil:
					summary.Failed++
				case f.Skipped:
					summary.Skipped++
				default:
					summary.Submitted++
				}
			}
		}
	} else {
		summary.Elapsed = time.Since(summary.StartedAt)
		msg := "NAS unreachable. Set offline.queueDbPath to enable field ingest."
		summary.Err = errors.New(msg)
		if notifyErr != nil {
			notifyErr("branchDAM Ingest", msg)
		}
	}

	r.mu.Lock()
	r.last = &summary
	r.mu.Unlock()

	return summary
}

func (r *Runner) setBusy(busy bool, cardPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = busy
	r.busyCard = cardPath
	if busy {
		r.busySince = time.Now()
	}
}

// Busy reports whether an ingest is in flight right now, without waiting
// for it -- cheap enough for the tray's 5s menu-refresh tick and the
// status page's per-request render.
func (r *Runner) Busy() (cardPath string, since time.Time, busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busyCard, r.busySince, r.busy
}

// TryLockIdle acquires the same gate TriggerIngest holds, WITHOUT
// blocking -- ok is false when an ingest is currently in flight. The
// caller MUST call the returned release func exactly once when done, and
// should hold it for the entire duration of whatever must not race a
// concurrent ingest (self-update's whole download-and-apply window, not
// just the instant of the check): sampling Busy() before a multi-minute
// download would still let a card inserted mid-download start an ingest
// that a binary swap then writes underneath. Holding gate for that whole
// window is what makes the invariant actually hold rather than merely be
// checked.
func (r *Runner) TryLockIdle() (release func(), ok bool) {
	if !r.gate.TryLock() {
		return nil, false
	}
	return r.gate.Unlock, true
}

// SetQueueDeps wires the offline-queue status readout and the drain/prune
// timers' targets (issue #32). Any of reader/drainer/pruner may be nil --
// see the Runner field doc comment above for why that is a normal,
// honestly-reported configuration, not an error condition.
func (r *Runner) SetQueueDeps(reader QueueReader, drainer Drainer, pruner Pruner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queueReader = reader
	r.drainer = drainer
	r.pruner = pruner
}

// TriggerDrain runs one internal/ingest.Drain pass via the configured
// Drainer, if one is wired and no other drain pass is currently running.
// drainMu is a DEDICATED mutex, deliberately never Runner.gate: gate is
// held for an ingest's or a self-update apply's entire duration, and a 5s
// drain timer sharing it would drop every tick during the exact window
// the queue is filling (an ingest in progress), while a drain pass holding
// gate across a slow NAS archive copy would block an inserted card from
// ingesting for minutes. Drain is stateless across calls (per-row
// next-attempt timestamps in queue.db are the real backoff), so skipping a
// tick outright when a previous pass is still running -- rather than
// queuing behind it -- is always safe and is what keeps concurrent passes
// from ever piling up. ran=false covers both "not configured" and "already
// running"; the caller (a timer tick or a menu click) treats both the same
// way: nothing to show beyond what Status() already reports.
func (r *Runner) TriggerDrain(ctx context.Context) (summary DrainSummary, ran bool) {
	if !r.drainMu.TryLock() {
		return DrainSummary{}, false
	}
	defer r.drainMu.Unlock()

	r.mu.Lock()
	drainer := r.drainer
	r.mu.Unlock()

	if drainer == nil {
		return DrainSummary{}, false
	}

	// Set inFlightDrain AFTER the nil-drainer guard. A periodic timer
	// tick on an unconfigured drainer must not flash "drain in
	// progress" to a concurrent Status() call; setting the flag here
	// ensures the flag is true only when an actual drain is running.
	r.mu.Lock()
	r.inFlightDrain = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlightDrain = false
		r.mu.Unlock()
	}()

	summary, err := drainer.Drain(ctx)
	summary.At = time.Now()
	if err != nil {
		summary.Err = err
	}

	r.mu.Lock()
	r.lastDrain = &summary
	r.mu.Unlock()

	return summary, true
}

// TriggerPrune runs one internal/prune.Pass pass via the configured
// Pruner, if one is wired and TryLockIdle succeeds. Unlike TriggerDrain,
// this DOES share Runner.gate (via TryLockIdle) with ingest and
// self-update: prune deletes from ingest.LocalEditRoot while an ingest
// writes into it, the real hazard a shared process introduces that
// `prune -watch` running standalone never had to guard against. Skipping
// a busy tick is safe at prune's own (much longer) cadence.
func (r *Runner) TriggerPrune(ctx context.Context) (summary PruneSummary, ran bool) {
	release, ok := r.TryLockIdle()
	if !ok {
		return PruneSummary{}, false
	}
	defer release()

	r.mu.Lock()
	pruner := r.pruner
	r.mu.Unlock()

	if pruner == nil {
		return PruneSummary{}, false
	}

	// Set inFlightPrune AFTER the nil-pruner guard. A menu-click
	// TriggerPrune on an unconfigured pruner must not briefly report
	// prune-in-flight to a concurrent Status() call; setting the flag
	// here ensures the flag is true only when an actual prune is running.
	r.mu.Lock()
	r.inFlightPrune = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlightPrune = false
		r.mu.Unlock()
	}()

	summary, err := pruner.Prune(ctx)
	summary.At = time.Now()
	if err != nil {
		summary.Err = err
	}

	r.mu.Lock()
	r.lastPrune = &summary
	r.mu.Unlock()

	return summary, true
}

// SetIntegrationSyncers swaps in the full set of registered integration
// syncers, built from the current config. Called from runTrayCmd at
// startup AND from configSettings.reload() on every settings change --
// unlike SetQueueDeps, this second call site is not optional: without it,
// rotating server.apiKey or changing server.baseUrl from the Settings menu
// would leave every integration syncer POSTing edges with a stale client
// indefinitely, the same staleness class of bug SetQueueDeps' own rebuild
// exists to fix for drain/prune. An absent map entry for a given ID is the
// honest "not configured" signal (see IntegrationStatus's own doc
// comment), never an error.
func (r *Runner) SetIntegrationSyncers(m map[IntegrationID]IntegrationSyncer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncers = m
}

// TriggerSync runs one Sync pass for id via the registered
// IntegrationSyncer, if one is wired and no other pass for the SAME id is
// currently running. ran=false covers both "not registered" (mirrors a nil
// Drainer/Pruner) and "already running" (mirrors TriggerDrain's own
// skip-don't-queue contract) -- a caller (a timer tick or a menu click)
// treats both identically: nothing new to show beyond what Status()
// already reports.
//
// Deliberately per-ID, not one shared mutex like drainMu: a Luminar sync in
// flight must never make a concurrent pass for a different integration
// (e.g. a future lrcat, issue #47) report skipped -- unrelated
// integrations must not block each other, only concurrent passes for the
// SAME id.
//
// Deliberately never shares Runner.gate either (unlike TriggerPrune): a
// sync only opens a third-party catalog read-only and POSTs edges -- it
// never touches ingest.ArchiveRoot or ingest.LocalEditRoot, so it has none
// of the hazard that makes TriggerPrune share gate with an ingest (prune
// deletes from LocalEditRoot while ingest writes into it). Sharing would
// instead cost twice: a scheduled sync tick dropped whenever a card
// happens to be mid-copy, and a slow catalog read blocking an inserted
// card from ingesting for its duration.
func (r *Runner) TriggerSync(ctx context.Context, id IntegrationID) (summary SyncSummary, ran bool) {
	r.mu.Lock()
	syncer := r.syncers[id]
	if syncer == nil {
		r.mu.Unlock()
		return SyncSummary{}, false
	}
	if r.syncInFlight[id] {
		r.mu.Unlock()
		return SyncSummary{}, false
	}
	r.syncInFlight[id] = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.syncInFlight, id)
		r.mu.Unlock()
	}()

	summary, err := syncer.Sync(ctx)
	summary.At = time.Now()
	if err != nil {
		summary.Err = err
	}

	r.mu.Lock()
	stamped := summary
	r.lastSync[id] = &stamped
	r.mu.Unlock()

	return summary, true
}

// SetHookInstallers swaps in the full set of registered hook installers.
// Mirrors SetIntegrationSyncers' own contract exactly (issue #60): an
// absent map entry for a given ID is the honest "not configured" signal,
// never an error.
func (r *Runner) SetHookInstallers(m map[HookID]HookInstaller) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hookInstallers = m
}

// SetHookState seeds Runner's CACHED view of id's on-disk state -- called
// once from runTrayCmd at startup, right after a one-time
// resolvehook.Detect call. Deliberately never called from Status() or the
// menu-refresh tick: Detect is a filesystem stat plus a checksum read on
// (potentially) a networked scriptsDir, and computing it live on every 5s
// refresh -- or on every status-page request -- would reproduce the exact
// hazard statusQueueReadTimeout (see Status' own doc comment) was added to
// fix. TriggerHookInstall keeps this cache fresh after an install action
// completes; nothing else ever needs to call this again.
func (r *Runner) SetHookState(id HookID, st HookState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamped := st
	r.hookState[id] = &stamped
}

// TriggerHookInstall installs id's script hook via the registered
// HookInstaller, if one is wired and no other install for the SAME id is
// currently running -- ran=false covers both cases, mirroring TriggerSync's
// own contract exactly. On success (or failure), the result is cached via
// the same map SetHookState seeds, so Status() (and a later PR's menu)
// reflect the fresh state without ever calling Detect live.
func (r *Runner) TriggerHookInstall(ctx context.Context, id HookID) (state HookState, ran bool) {
	r.mu.Lock()
	installer := r.hookInstallers[id]
	if installer == nil {
		r.mu.Unlock()
		return HookState{}, false
	}
	if r.hookInFlight[id] {
		r.mu.Unlock()
		return HookState{}, false
	}
	r.hookInFlight[id] = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.hookInFlight, id)
		r.mu.Unlock()
	}()

	state, err := installer.Install(ctx)
	state.At = time.Now()
	if err != nil {
		state.Err = err
	}

	r.mu.Lock()
	stamped := state
	r.hookState[id] = &stamped
	r.mu.Unlock()

	return state, true
}

// RevealHook opens id's Scripts folder via its registered HookInstaller --
// a fire-and-forget OS shell-out, same "no meaningful error surface"
// precedent as run_supported.go's own openBrowser (used for "Open status
// page"): Reveal never mutates hook state, so unlike TriggerHookInstall it
// needs neither hookInFlight tracking nor a SetHookState cache update, and
// a caller is free to discard the error the same way openBrowser's own
// caller does.
func (r *Runner) RevealHook(id HookID) error {
	r.mu.Lock()
	installer := r.hookInstallers[id]
	r.mu.Unlock()
	if installer == nil {
		return fmt.Errorf("tray: no hook installer registered for %q", id)
	}
	return installer.Reveal()
}

// SetOnCardIngested registers a callback invoked when a card is detected
// and ingested by the Detector.Watch goroutine (run_supported.go uses this
// to trigger an immediate menu refresh).
func (r *Runner) SetOnCardIngested(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onCardIngested = fn
}

// BaseContext returns the registered lifecycle context for the detector
// goroutine, or context.Background() if none was set.
func (r *Runner) BaseContext() context.Context {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()
	if r.detectorBaseCtx != nil {
		return r.detectorBaseCtx
	}
	return context.Background()
}

// SetDetectorInterval sets the poll interval used when creating a new
// ingest.Detector. If d <= 0, ingest.DefaultPollInterval is used.
func (r *Runner) SetDetectorInterval(d time.Duration) {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()
	if d <= 0 {
		d = ingest.DefaultPollInterval
	}
	r.detectorInterval = d
}

// SetDetectorRequireDCIM sets whether the card detector requires a DCIM/
// subdirectory to detect a volume.
func (r *Runner) SetDetectorRequireDCIM(v bool) {
	r.detectorMu.Lock()
	changed := r.detectorRequireDCIM != v
	r.detectorRequireDCIM = v
	running := r.detectorCancel != nil
	r.detectorMu.Unlock()

	if changed && running {
		r.mu.Lock()
		roots := append([]string(nil), r.watchDirs...)
		r.mu.Unlock()
		r.ReconfigureDetector(r.BaseContext(), roots)
	}
}

// SetDetectorErrorHandler registers a handler for non-cancellation errors
// returned by the Detector.Watch loop (run_supported.go forwards these to errCh).
func (r *Runner) SetDetectorErrorHandler(fn func(err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detectorErrHandler = fn
}

// ReconfigureDetector stops any currently running Detector.Watch goroutine,
// waits for it to exit, and if roots is non-empty, starts a new Detector.Watch
// goroutine polling roots (issue #78). If ctx is non-nil, it becomes the parent context
// for the new detector goroutine (and future ReconfigureDetector calls);
// if ctx is nil, the previously registered parent context (or context.Background())
// is used.
func (r *Runner) ReconfigureDetector(ctx context.Context, roots []string) {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()

	if ctx != nil {
		r.detectorBaseCtx = ctx
	}
	baseCtx := r.detectorBaseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}

	if r.detectorCancel != nil {
		r.detectorCancel()
		if r.detectorDone != nil {
			<-r.detectorDone
		}
		r.detectorCancel = nil
		r.detectorDone = nil
	}

	r.mu.Lock()
	r.watchDirs = append([]string(nil), roots...)
	interval := r.detectorInterval
	if interval <= 0 {
		interval = ingest.DefaultPollInterval
	}
	r.mu.Unlock()

	if len(roots) == 0 {
		return
	}

	dctx, cancel := context.WithCancel(baseCtx)
	r.detectorCancel = cancel
	done := make(chan struct{})
	r.detectorDone = done

	detector := ingest.NewDetector(roots, interval, r.detectorRequireDCIM)
	go func() {
		defer close(done)
		err := detector.Watch(dctx, func(diff ingest.Diff) {
			for _, path := range diff.Inserted {
				r.TriggerIngest(dctx, path)
				r.mu.Lock()
				cb := r.onCardIngested
				r.mu.Unlock()
				if cb != nil {
					cb()
				}
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			r.mu.Lock()
			handler := r.detectorErrHandler
			r.mu.Unlock()
			if handler != nil {
				handler(err)
			}
		}
	}()
}

// StopDetector cancels any running Detector.Watch goroutine and waits for it
// to exit.
func (r *Runner) StopDetector() {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()

	if r.detectorCancel != nil {
		r.detectorCancel()
		if r.detectorDone != nil {
			<-r.detectorDone
		}
		r.detectorCancel = nil
		r.detectorDone = nil
	}
}

// Reconfigure swaps in a freshly built Ingester plus the watch/scratch
// description it should report going forward -- the guarded-rebuild
// mechanism issue #31's settings menu applies every hot-reloadable config
// change through, rather than giving each individual field (server URL,
// API key, archive/local roots, naming template, requireUnbuffered) its
// own bespoke hot-patch path. Blocks until no ingest is in flight (reuses
// TriggerIngest's own gate), so a config reload can never race a card
// mid-copy the way swapping these fields without synchronization would.
//
// When watchDirs differs from the current roots, Reconfigure invokes
// ReconfigureDetector (issue #78) AFTER releasing gate, so waiting on the
// previous detector goroutine to exit cannot deadlock against an in-flight
// TriggerIngest call.
//
// One field is deliberately NOT reconfigurable this way and is the
// caller's responsibility to treat as restart-required instead:
// tray.statusAddr (the embedded HTTP server's Listen() call already
// happened and is this tray's single-instance guard -- there's nothing to
// swap it into).
func (r *Runner) Reconfigure(ingester Ingester, watchDirs []string, scratchDir string) {
	var rootsChanged bool
	var newRoots []string

	r.gate.Lock()
	r.mu.Lock()
	oldRoots := append([]string(nil), r.watchDirs...)
	r.ingester = ingester
	r.scratchDir = scratchDir
	if !slices.Equal(oldRoots, watchDirs) {
		rootsChanged = true
		newRoots = append([]string(nil), watchDirs...)
		r.watchDirs = newRoots
	}
	r.mu.Unlock()
	r.gate.Unlock()

	if rootsChanged {
		r.ReconfigureDetector(r.BaseContext(), newRoots)
	}
}

// statusQueueReadTimeout bounds Status()'s QueueReader.Counts call -- see
// that call site's comment for why this exists.
const statusQueueReadTimeout = 5 * time.Second

// Status returns a snapshot of the current state for the status page and
// the tray tooltip. selfUpdate is passed in rather than computed here --
// self-update's own check is async and gated by config the caller already
// has, so Runner stays a pure ingest-facing type rather than reaching
// into an unrelated subsystem.
func (r *Runner) Status(selfUpdate UpdateStatus) Status {
	r.mu.Lock()
	last := r.last
	busy := r.busy
	busyCard := r.busyCard
	busySince := r.busySince
	watchDirs := append([]string(nil), r.watchDirs...)
	scratchDir := r.scratchDir
	queueReader := r.queueReader
	pruneEnabled := r.pruner != nil
	lastDrain := r.lastDrain
	lastPrune := r.lastPrune
	inFlightDrain := r.inFlightDrain
	inFlightPrune := r.inFlightPrune

	// Built entirely under r.mu, not after unlocking: r.syncers/r.lastSync
	// are read map entries here, not copied whole-map references, so a
	// concurrent TriggerSync write (a single map-key assignment, not a
	// map replace) could otherwise race this read -- see TriggerSync's
	// own r.lastSync[id] = &stamped assignment.
	integrations := make([]IntegrationStatus, 0, len(Integrations()))
	for _, d := range Integrations() {
		st := IntegrationStatus{ID: d.ID, Registered: r.syncers[d.ID] != nil}
		if ls, ok := r.lastSync[d.ID]; ok {
			st.LastSync = ls
		}
		integrations = append(integrations, st)
	}

	// Built under the same lock, same reasoning as integrations above:
	// r.hookState is read map entries here, not a copied whole-map
	// reference, so a concurrent TriggerHookInstall write could otherwise
	// race this read.
	hooks := make([]HookStatus, 0, len(Hooks()))
	for _, id := range Hooks() {
		hs := HookStatus{ID: id, Registered: r.hookInstallers[id] != nil}
		if s, ok := r.hookState[id]; ok {
			hs.State = s
		}
		hooks = append(hooks, hs)
	}
	r.mu.Unlock()

	scratchNote := "not configured"
	if scratchDir != "" {
		scratchNote = fmt.Sprintf("%s (usage tracking not yet implemented)", scratchDir)
	}

	qs := QueueStatus{PruneEnabled: pruneEnabled, LastDrain: lastDrain, LastPrune: lastPrune}
	if queueReader != nil {
		qs.Configured = true
		// Status() is called from run_supported.go's 5s menu-refresh tick
		// and from every status-page request -- unlike TriggerDrain/
		// TriggerPrune's own periodicPassTimeout-bounded passes, an
		// unbounded read here would hang the whole tray menu (including
		// Quit) on a wedged or NAS-backed queue.db, not just show a stale
		// number. statusQueueReadTimeout matches queue.Store's own
		// busy_timeout=5000 pragma (Hermes review finding on this PR).
		ctx, cancel := context.WithTimeout(context.Background(), statusQueueReadTimeout)
		counts, err := queueReader.Counts(ctx)
		cancel()
		if err != nil {
			qs.Err = err
		} else {
			qs.Counts = counts
		}
	}

	var lastIngestWasOffline bool
	if last != nil && last.Offline {
		lastIngestWasOffline = true
	}
	var pendingOfflineCount int
	if qs.Configured && qs.Err == nil {
		pendingOfflineCount = qs.Counts.Pending()
	}

	return Status{
		WatchDirs:            watchDirs,
		ScratchNote:          scratchNote,
		QueueStatus:          qs,
		LastIngest:           last,
		SelfUpdate:           selfUpdate,
		Busy:                 busy,
		BusyCard:             busyCard,
		BusySince:            busySince,
		HandshakeOK:          lastDrain != nil && lastDrain.HandshakeOK,
		HasDrained:           lastDrain != nil,
		InFlightDrain:        inFlightDrain,
		InFlightPrune:        inFlightPrune,
		Integrations:         integrations,
		Hooks:                hooks,
		LastIngestWasOffline: lastIngestWasOffline,
		PendingOfflineCount:  pendingOfflineCount,
	}
}
