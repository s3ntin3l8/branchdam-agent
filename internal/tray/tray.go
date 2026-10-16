// Package tray is the tray-resident shell around internal/ingest.Engine
// (issue #3, M1's tray half). Per the plan doc's "UI stack (M1)" section
// and this repo's AGENTS.md ingest-core comment, the ingest core has no UI
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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/eject"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/netgate"
)

// ErrUnsupported is returned by Run on any platform other than
// windows/darwin. The tray is scoped to those two per the plan doc and
// issue #3; a Linux workstation still has the fully-tested headless
// `ingest`/`ingest --watch` path, just no tray icon.
var ErrUnsupported = errors.New("tray: unsupported on this platform (windows and darwin only); use `branchdam-agent ingest` instead")

// ErrConfigIncomplete is returned by triggerIngest when required config
// fields are missing and the agent cannot safely ingest.
var ErrConfigIncomplete = errors.New("tray: config incomplete — cannot ingest until all required fields are set")

// IngestGate decides whether to proceed with ingesting a detected card volume (issue #79).
// Confirm returns proceed=true to proceed with ingest, or proceed=false/error to skip.
type IngestGate interface {
	Confirm(ctx context.Context, volumePath, volumeName string) (proceed bool, err error)
}

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
	CardPath  string        `json:"cardPath"`
	StartedAt time.Time     `json:"startedAt"`
	Elapsed   time.Duration `json:"elapsed"`
	Submitted int           `json:"submitted"`
	// Resumed counts offline-ingest files an earlier, interrupted run had
	// already copied and queued (same source path, size and mtime) -- kept
	// apart from Submitted so a re-run over a half-ingested card doesn't
	// read as fresh imports. Always 0 for an online ingest.
	Resumed int `json:"resumed"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	// Deferred counts files whose local copy landed but whose upload was
	// held back on a metered connection (ingest.FileResult.Deferred) --
	// they still exist only on the card and the local edit SSD, so the
	// card is not done and must not be ejected.
	Deferred int   `json:"deferred"`
	Err      error `json:"-"`
	Offline  bool  `json:"offline"`
	// Canceled is true when the operator stopped this ingest
	// (Runner.CancelIngest) before it walked the whole card. The counts
	// above cover the files handled before the stop; Err wraps
	// ingest.ErrCanceled, so OK() is false and the card is never
	// auto-ejected. Re-running the ingest picks up where it stopped.
	Canceled bool `json:"canceled,omitempty"`
}

// MarshalJSON renders Err as its message string rather than encoding the
// error value directly -- a bare `error`-typed field silently marshals to
// "{}" for most error implementations, since they expose no fields of
// their own for encoding/json to see (see statusapi.go's errString doc
// comment, which this reuses). Every other field is tagged camelCase to
// match the rest of Status's JSON surface -- this is the only nested
// summary type Status.LastIngest ever reaches app.js by field name, so
// getting the casing right here (rather than leaving it at its untagged
// PascalCase default like this file's other summary/state types) is what
// keeps the one JSON consumer this PR adds internally consistent. Err is
// tagged `json:"-"` on the struct itself and re-added explicitly below at
// the lowercase "err" key, alongside every other field via the embedded
// alias.
func (s IngestSummary) MarshalJSON() ([]byte, error) {
	type alias IngestSummary
	return json.Marshal(struct {
		alias
		Err string `json:"err,omitempty"`
	}{alias: alias(s), Err: errString(s.Err)})
}

// OK reports whether every file that should have been submitted succeeded
// (a deliberate sidecar skip does not count as a failure) and none is still
// waiting on the card for an unmetered connection -- mirrors
// cmd/branchdam-agent/ingest.go's printIngestReport logic exactly, since
// the tray drives the same underlying Engine and should reach the same
// verdict from the same CardResult shape.
func (s IngestSummary) OK() bool {
	return s.Err == nil && s.Failed == 0 && s.Deferred == 0
}

// shouldAutoEject is triggerIngest's whole eject decision, kept pure so it
// can be table-tested. Ejecting is only ever right for a camera card the
// detector found and the operator (or the autoImportPaths allow-list)
// accepted: isDetection must be true and cardPath must be exactly a card
// volume root (an immediate subdirectory of a cardRoots parent that holds
// no ingest destination -- see isCardVolumeRoot). "Ingest now", "Import
// from folder…" and POST /api/actions/ingest are manual (isDetection
// false) and never eject, whatever path they were given -- a picked folder
// on the edit SSD or the NAS must not take its whole disk offline. And the
// ingest itself must be OK(): nothing failed and nothing deferred.
func shouldAutoEject(autoEject, isDetection bool, cardPath string, cardRoots []string, destRoots []string, summary IngestSummary) bool {
	return autoEject && isDetection && summary.OK() && isCardVolumeRoot(cardPath, cardRoots, destRoots...)
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
	Enabled   bool
	Checked   bool
	CheckedAt time.Time
	// Phase describes the active self-update lifecycle. It is deliberately
	// separate from Note so the native window and tray can render progress
	// without parsing prose. Empty means the updater has not started a pass.
	Phase          string
	StartedAt      time.Time
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
	// RolledBack is set together with Applied (and Phase restarting) when
	// the swap that just completed was a rollback to the previous
	// version rather than a forward update, so the restart is logged as
	// "rolled back to" (Outcome.RolledBack).
	RolledBack bool
	// RollbackVersion is the version a rollback would restore, or "" when
	// no previous version is on disk -- filled from
	// SelfUpdater.RollbackAvailable for the window's "Roll back" button.
	RollbackVersion string
}

// Update lifecycle phase values shared by the tray menu, status API, and the
// command-layer self-update implementation. They are wire strings as well as
// display-state identifiers, so callers should compare these constants
// rather than repeating literals.
const (
	UpdatePhaseIdle        = "idle"
	UpdatePhaseChecking    = "checking"
	UpdatePhaseAvailable   = "available"
	UpdatePhaseDownloading = "downloading"
	UpdatePhaseVerifying   = "verifying"
	UpdatePhaseRestarting  = "restarting"
	UpdatePhaseFailed      = "failed"
	// UpdatePhaseRollingBack covers a window-started rollback restoring
	// the previous version's files (the tray menu's own rollback tracks
	// this in its select loop instead).
	UpdatePhaseRollingBack = "rolling-back"
)

// MarshalJSON renders Err as a string -- see IngestSummary.MarshalJSON's
// doc comment for why and how.
func (u UpdateStatus) MarshalJSON() ([]byte, error) {
	type alias UpdateStatus
	return json.Marshal(struct {
		alias
		Err string `json:"Err,omitempty"`
	}{alias: alias(u), Err: errString(u.Err)})
}

// Note renders UpdateStatus as the one-line string the status page shows
// -- the same text startSelfUpdateCheck used to compute directly before
// this type existed.
func (u UpdateStatus) Note() string {
	switch {
	case !u.Enabled:
		return "disabled (selfUpdate.enabled: false in config)"
	case u.Phase != "" && u.Phase != UpdatePhaseAvailable && u.Phase != UpdatePhaseIdle && u.Phase != UpdatePhaseRestarting:
		if u.Err != nil {
			return fmt.Sprintf("%s: %v", u.Phase, u.Err)
		}
		return u.Phase
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
	WatchDirs    []string       `json:"watchDirs"`
	ScratchUsage ScratchUsage   `json:"scratchUsage"`
	QueueStatus  QueueStatus    `json:"queueStatus"`
	LastIngest   *IngestSummary `json:"lastIngest,omitempty"`
	SelfUpdate   UpdateStatus   `json:"selfUpdate"`
	// Paused reflects Runner.Paused() (issue #83) -- manual ingest pause.
	Paused bool `json:"paused"`
	// Busy and BusyCard reflect Runner.Busy() -- shown on the status page
	// and used by the tray menu to disable "Install and restart" while an
	// ingest is running (see Runner.TryLockIdle for the actual gate this
	// only mirrors for display).
	Busy     bool   `json:"busy"`
	BusyCard string `json:"busyCard,omitempty"`
	// BusySince is when the current ingest started (zero when not
	// busy; reset to the zero value at ingest completion, not left
	// stale at the last start time, so the status page's "Running…
	// since HH:MM:SS" line cleanly hides the indicator when the
	// tray is idle -- issue #109 / audit B-17 follow-up).
	BusySince time.Time `json:"busySince,omitempty"`
	// IngestProgress is the most recent progress sample from an in-flight
	// ingest (DualWrite/WriteLocal/Verify), or nil when idle.
	IngestProgress *ingest.ProgressEvent `json:"ingestProgress,omitempty"`
	// IngestCancelRequested is true once Runner.CancelIngest has asked the
	// in-flight ingest to stop; it is still finishing its current file.
	IngestCancelRequested bool `json:"ingestCancelRequested,omitempty"`
	// HandshakeOK surfaces the most recent DrainSummary.HandshakeOK to the
	// status page -- the operator's most basic "is the server reachable?"
	// signal, which DrainSummary already computes but never exposed. False
	// when no drain has run yet (see HasDrained for that distinction);
	// this is the single source of truth for the "Server connection"
	// section's reachable/unreachable label, so the template does not
	// reach into QueueStatus.LastDrain directly.
	HandshakeOK bool `json:"handshakeOk"`
	// LastHandshakeAt is the timestamp of the most recent successful
	// drain pass. Zero means "never drained" -- the status page
	// template suppresses the line in that case. Mirrors
	// DrainSummary.LastHandshakeAt (issue #109 follow-up).
	LastHandshakeAt time.Time `json:"lastHandshakeAt,omitempty"`
	// HasDrained is true once at least one drain pass has completed in
	// this session -- the "have we ever heard from the server?" signal
	// the Server connection section uses to distinguish "no drain run
	// yet" from "last drain: handshake failed". Independent of
	// HandshakeOK on purpose: a never-drained install is neither
	// "reachable" nor "unreachable", it's "unknown".
	HasDrained bool `json:"hasDrained"`
	// InFlightDrain reports whether a drain pass is currently running.
	InFlightDrain bool `json:"inFlightDrain"`
	// InFlightPrune reports whether a prune pass is currently running.
	InFlightPrune bool `json:"inFlightPrune"`
	// InFlightProbe reports whether a server-reachability check
	// (TriggerServerProbe) is currently running. Not consumed by the
	// Wails window today (it tracks "Test connection" busy state
	// client-side, in its own inFlightActions set), but kept for parity
	// with InFlightDrain/InFlightPrune above -- any future consumer of
	// Status() (a tray menu item, a second window) gets it for free.
	InFlightProbe bool `json:"inFlightProbe"`
	// Integrations is RUNTIME state only, ordered by the compile-time
	// Integrations() registry so the status page and (a later PR's) menu
	// render in the same order every time. Config state (enabled, dry
	// run, configured paths) comes from Settings.Snapshot(), never from
	// here -- Runner never reads config.
	Integrations []IntegrationStatus `json:"integrations,omitempty"`
	// Hooks is Integrations' counterpart for installable script hooks
	// (DaVinci Resolve's render hook, issue #60) -- a SEPARATE list, not
	// folded into Integrations, since a hook has no CatalogSyncConfig and
	// isn't in Integrations()'s own registry (see HookID's own doc
	// comment). Ordered by the compile-time Hooks() registry.
	Hooks []HookStatus `json:"hooks,omitempty"`
	// ConfigIncomplete is true when required config fields are missing
	// (server.apiKey, server.baseUrl, ingest.archiveRoot, etc.). The
	// tray starts anyway -- it shows a "not configured" icon and menu
	// state, and the user configures through the Settings menu.
	ConfigIncomplete bool     `json:"configIncomplete"`
	MissingFields    []string `json:"missingFields,omitempty"`
	// ConfigRevision increments every time the running config is (re)applied
	// (startup and every reload). The window compares it with the revision
	// its settings form was loaded at, so a change made elsewhere (a
	// deep-link pair in the tray, a hand edit + reload) refreshes the form.
	ConfigRevision int64 `json:"configRevision"`
	// ServerProbe is the most recent on-demand "Test connection" result
	// (nil if none has run yet this session, including the automatic
	// startup/periodic checks -- see cmd/branchdam-agent's serverProbeInterval
	// wiring). Deliberately independent of HandshakeOK/HasDrained above:
	// those two require offline.queueDbPath to be configured at all (see
	// QueueStatus.Configured's own doc comment); ServerProbe does not, and
	// is what the status page renders as the Server card's reachability
	// pill instead.
	ServerProbe *ProbeResult `json:"serverProbe,omitempty"`
	// UploadPausedMetered is true when ingest.pauseUploadOnMetered is on
	// and the last metered probe reported a metered connection (or
	// failed -- shown fail-closed, like the drain gate). Read from an
	// asynchronously refreshed cache so Status() never blocks on the
	// probe; see metered.go. Display only: TriggerDrain probes for itself.
	UploadPausedMetered bool `json:"uploadPausedMetered"`
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
	// gateHeld mirrors the reservation state for the select loop's quit
	// guard. It is separate from Busy(): a self-update owns gate before any
	// ingest status is published, so Busy/UpdateStatus snapshots have a
	// small but real gap.
	gateHeld atomic.Bool

	// reconfigMu guards reconfigPending/reconfigWaiting: the newest
	// Reconfigure call not yet applied (it waits for gate while an ingest
	// or a self-update holds it), and whether a goroutine is already
	// waiting to apply it. Never held across gate.
	reconfigMu      sync.Mutex
	reconfigPending *pendingReconfig
	reconfigWaiting bool

	// drainMu is a SEPARATE mutex from gate, deliberately -- see
	// TriggerDrain's doc comment for why a drain pass must never be
	// blocked by (or block) an ingest or a self-update apply.
	drainMu sync.Mutex

	// probeMu is its own dedicated mutex, same reasoning as drainMu: a
	// server-reachability check must never be blocked by (or block) an
	// ingest, a drain/prune pass, or a self-update apply. See
	// TriggerServerProbe's own doc comment for why it is also
	// deliberately not gated on ConfigIncomplete the way TriggerDrain/
	// TriggerPrune are.
	probeMu sync.Mutex

	// telemetry is the POST /api/v1/agent/telemetry wiring -- see
	// telemetry.go.
	telemetry telemetryDeps

	// scratchUsageMu is its own dedicated mutex, same reasoning as
	// drainMu/probeMu above -- but here the concern isn't lock ordering
	// with an ingest/drain/update, it's that Status() must NEVER call the
	// underlying OS stat (diskUsage) synchronously at all: statfs(2)/
	// GetDiskFreeSpaceEx on an unmounted network share can hang
	// uninterruptibly, with no context to bound it the way
	// statusQueueReadTimeout bounds the queue.db read below -- and
	// Status() runs on every tray menu refresh tick (5s) AND every
	// status-page request, so a single hung scratch-dir stat would freeze
	// the whole tray menu, including Quit. refreshScratchUsage is the
	// only writer; scratchUsageSnapshot (Status()'s own reader) only ever
	// returns the last completed reading and kicks a background refresh,
	// never blocks on one.
	scratchUsageMu sync.Mutex
	// scratchUsageDir is the path the CURRENT scratchUsage/scratchUsageAt
	// reading is actually for -- compared against the live scratchDir on
	// every read (not just a TTL check) so a Reconfigure to a new
	// localEditRoot can never show the OLD path's numbers relabeled as
	// the new path's, even transiently within the staleness window.
	scratchUsageDir string
	scratchUsage    ScratchUsage
	scratchUsageAt  time.Time
	// scratchUsageInFlightDir is the dir a refresh goroutine is CURRENTLY
	// running for, or "" when none is -- keyed by directory, not a bare
	// bool, so a stat hung on some OLD (possibly unreachable) path can
	// never block a refresh for a NEW one after a Reconfigure: the whole
	// point of this async design is to survive a stat that never returns,
	// and a single shared in-flight flag would let exactly that hang
	// silently freeze visibility of a brand new, perfectly healthy path
	// too (review finding on this PR). refreshScratchUsage additionally
	// checks this field still names ITS OWN dir before writing its
	// result, so a late-arriving result from an abandoned dir can never
	// clobber whatever the CURRENT dir's own refresh already wrote.
	scratchUsageInFlightDir string

	// paused tracks whether manual ingest pause is active (shoot-mode, issue #83).
	// Session-only, never persisted.
	paused atomic.Bool
	// pausedInserts holds card volumes the detector reported as inserted
	// while paused (guarded by mu). The detector only reports a volume
	// once per insertion, so without this a card inserted mid-pause was
	// never offered at all; SetPaused(false) offers the ones still
	// present (see offerCardsHeldDuringPause).
	pausedInserts map[string]bool

	// detectedVolumes is the volume set the detector last reported
	// (inserted and not since removed), guarded by mu. A restarted
	// detector (ReconfigureDetector) starts from it, so a reload while
	// paused or not never reports an already-handled card as a new
	// insertion. "Handled" means reported, whatever the ingest outcome
	// (config incomplete, a failed IngestCard, a gate error): a reload is
	// not an insertion. Re-inserting the card or "Ingest now" offers it
	// again.
	detectedVolumes map[string]bool

	// mu guards every field below, including ingester/watchDirs/
	// scratchDir -- unlike before issue #31's settings menu, these are no
	// longer set once at construction and left alone; Reconfigure can
	// swap them at any time the tray is running.
	mu            sync.Mutex
	ingester      Ingester
	watchDirs     []string
	scratchDir    string // LocalEditRoot -- see scratchUsageMu's own block above for how its disk usage is measured.
	last          *IngestSummary
	busy          bool
	busyCard      string
	busySince     time.Time
	onPauseChange func(paused bool)
	// ingestStop asks the in-flight ingest to stop at the next file
	// boundary (ingest.WithFileBoundaryCancel); nil while no ingest holds
	// gate. ingestStopRequested records that CancelIngest called it, for
	// Status().IngestCancelRequested ("stopping after the current file").
	ingestStop          context.CancelFunc
	ingestStopRequested bool

	// lastProgress records the most recent progress sample from an in-flight
	// IngestCard call (DualWrite/WriteLocal/Verify). Cleared when idle.
	lastProgress atomic.Pointer[ingest.ProgressEvent]

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

	// probe/lastProbe/inFlightProbe mirror drainer/lastDrain/inFlightDrain's
	// own shape (see SetServerProbe/TriggerServerProbe), but wired
	// independently of the offline queue: probe is set whenever the server
	// is configured at all (server.baseUrl/apiKey/agentId), not only when
	// offline.queueDbPath is also set. Re-registered on every settings
	// reload, unlike queueReader/drainer/pruner, since rotating
	// server.apiKey or server.baseUrl must not leave this probing a stale
	// client indefinitely -- the same staleness class SetIntegrationSyncers'
	// own doc comment describes for issue #57.
	probe         ServerProbe
	lastProbe     *ProbeResult
	inFlightProbe bool

	// onSuccessfulHandshake is invoked after every TriggerDrain pass
	// whose HandshakeOK is true, with the just-stamped
	// summary.LastHandshakeAt as its argument. Wired by runTrayCmd to
	// internal/runtime.Save so the freshness signal the status page
	// renders as "last handshake: <since> ago" survives a tray
	// restart (issue #149 / audit F-13 cross-session half). The
	// callback is allowed to return an error -- failing to write
	// runtime.json (disk full, permission revocation) must not block
	// the drain pass, and a panicking callback is caught and
	// recovered by the TriggerDrain hook site. Both the field and the
	// setter/seed follow the same one-time-wired-after-construction
	// shape as SetQueueDeps and SetErrorNotifier: the drain timer
	// calls into this hook from a goroutine the tray owns, so a
	// mid-flight swap is rare but not undefined, and the setter takes
	// r.mu to be consistent with the rest of the Runner's setters.
	//
	// Stored in an atomic.Pointer so the TriggerDrain hook site can
	// read it without holding r.mu (the save may be slow on a
	// network share; holding r.mu across a slow Save would block
	// the next drain pass's r.lastDrain write and the carry-forward
	// key off r.lastDrain, breaking Key Invariant 9's
	// serialization contract). An atomic read is a free snapshot
	// of the current callback, with no torn-write window during a
	// SetOnSuccessfulHandshake swap.
	onSuccessfulHandshake atomic.Pointer[func(time.Time) error]

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

	// ingestGate gates card ingest on detection (issue #79). Nil means
	// always proceed (used by tests and headless CLI).
	ingestGate IngestGate
	// skipped holds the in-memory session-scoped ignore set of volumes
	// the operator dismissed with "Skip this time" (issue #79).
	skipped map[string]bool
	// notifier emits user-facing OS desktop notifications (issue #79).
	notifier func(title, message string)
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
	detectorMu       sync.Mutex
	detectorCancel   context.CancelFunc
	detectorDone     chan struct{}
	detectorBaseCtx  context.Context
	detectorInterval time.Duration
	// detectorRunningInterval and detectorRunningRequireDCIM are the poll
	// interval and DCIM requirement the running detector was built with
	// (guarded by detectorMu; meaningful only while detectorCancel !=
	// nil). A reload that changes only ingest.pollIntervalSecs or
	// ingest.requireDCIM restarts the detector when its Reconfigure is
	// applied, never while an ingest holds the gate -- see detectorStale.
	detectorRunningInterval    time.Duration
	detectorRunningRequireDCIM bool

	// detectorRequireDCIM is the latest SetDetectorRequireDCIM value
	// (guarded by detectorMu). "Ingest now" and the resume offer read it
	// at once; a running detector picks it up on its deferred restart.
	detectorRequireDCIM  bool
	onCardIngested       func()
	detectorErrHandler   func(err error)
	pauseUploadOnMetered bool
	isMeteredFn          func() (bool, error)
	// meteredCache is the display-only, asynchronously refreshed copy of
	// the metered probe -- see metered.go.
	meteredCache meteredCache

	// autoEject gates OS-level safe eject after verified ingest (issue #87).
	autoEject bool
	ejectFn   func(mountPath string) error

	// configIncomplete tracks whether required config fields are missing.
	// The tray starts anyway in "setup mode" -- ingest is blocked, and
	// the icon/menu show a "not configured" state.
	configIncomplete bool
	missingFields    []string
	configRevision   int64

	// confirmDestructive controls whether destructive menu actions
	// (drain, prune, install-and-restart, rollback) show a confirmation
	// dialog. Updated live via SetConfirmDestructive when the operator
	// toggles the Settings checkbox.
	confirmDestructive bool
}

// NewRunner builds a Runner over ingester, describing watchDirs (typically
// the configured ingest.cardRoots) and scratchDir (ingest.localEditRoot)
// in the status snapshot.
func NewRunner(ingester Ingester, watchDirs []string, scratchDir string) *Runner {
	r := &Runner{
		ingester:         ingester,
		watchDirs:        append([]string(nil), watchDirs...),
		scratchDir:       scratchDir,
		detectorInterval: ingest.DefaultPollInterval,
		skipped:          map[string]bool{},
		syncInFlight:     map[IntegrationID]bool{},
		lastSync:         map[IntegrationID]*SyncSummary{},
		hookInFlight:     map[HookID]bool{},
		hookState:        map[HookID]*HookState{},
		ejectFn:          eject.Eject,
	}
	r.wireProgress(ingester)
	return r
}

// Paused reports whether manual ingest pause is active (shoot-mode, issue #83).
func (r *Runner) Paused() bool {
	return r.paused.Load()
}

// SetPaused updates the manual ingest pause state and invokes onPauseChange if registered.
// Resuming (true -> false) also offers every card volume that was inserted
// while paused and is still mounted, through the same IngestGate path a
// live detection takes -- see offerCardsHeldDuringPause.
func (r *Runner) SetPaused(v bool) {
	was := r.paused.Swap(v)
	r.mu.Lock()
	cb := r.onPauseChange
	r.mu.Unlock()
	if cb != nil {
		cb(v)
	}
	if was && !v {
		r.offerCardsHeldDuringPause()
	}
}

// holdPausedInserts records volumes the detector reported while paused,
// and forgets removed ones so a card pulled mid-pause is never offered.
// Reports whether diff.Inserted was held (the tray was paused). The pause
// check and the record happen under one r.mu critical section, and
// offerCardsHeldDuringPause takes the set under r.mu after flipping
// paused, so an insertion racing a resume is offered exactly once: either
// held here and offered by the resume, or not held and ingested by the
// detector itself.
func (r *Runner) holdPausedInserts(diff ingest.Diff) (held bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range diff.Removed {
		delete(r.pausedInserts, v)
	}
	if !r.paused.Load() {
		return false
	}
	for _, v := range diff.Inserted {
		if r.pausedInserts == nil {
			r.pausedInserts = map[string]bool{}
		}
		r.pausedInserts[v] = true
	}
	return true
}

// recordDetected keeps detectedVolumes in step with the detector's
// reports; see that field.
func (r *Runner) recordDetected(diff ingest.Diff) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range diff.Removed {
		delete(r.detectedVolumes, v)
	}
	for _, v := range diff.Inserted {
		if r.detectedVolumes == nil {
			r.detectedVolumes = map[string]bool{}
		}
		r.detectedVolumes[v] = true
	}
}

// offerCardsHeldDuringPause runs, in its own goroutine, a detection-style
// ingest (TriggerDetectedIngest: skip set, IngestGate confirmation or
// allow-list) for every volume held by holdPausedInserts that the current
// card-volume selection still reports as present. Asynchronous because
// SetPaused is called from the tray's menu loop and from the status API;
// the volume listing and the gate's dialog must block neither. A volume
// that was already present before the pause is not re-offered -- the
// detector reported it (and it was handled) before pausing. If the tray is
// paused again part-way through, the remaining volumes are held again.
func (r *Runner) offerCardsHeldDuringPause() {
	r.mu.Lock()
	held := r.pausedInserts
	r.pausedInserts = nil
	r.mu.Unlock()
	if len(held) == 0 {
		return
	}
	go func() {
		r.mu.Lock()
		cardRoots := append([]string(nil), r.watchDirs...)
		destRoots := []string{r.archiveRoot, r.scratchDir}
		r.mu.Unlock()
		r.detectorMu.Lock()
		requireDCIM := r.detectorRequireDCIM
		ctx := r.detectorBaseCtx
		r.detectorMu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}

		present, _ := selectCardVolumes(cardRoots, requireDCIM, destRoots...)
		var offer []string
		for _, v := range present {
			if held[v] {
				offer = append(offer, v)
			}
		}
		for i, v := range offer {
			if r.paused.Load() && r.holdPausedInserts(ingest.Diff{Inserted: offer[i:]}) {
				return
			}
			r.TriggerDetectedIngest(ctx, v)
			r.mu.Lock()
			cb := r.onCardIngested
			r.mu.Unlock()
			if cb != nil {
				cb()
			}
		}
	}()
}

// SetOnPauseChange registers a callback invoked when the pause state changes.
func (r *Runner) SetOnPauseChange(fn func(paused bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onPauseChange = fn
}

// SetConfigIncomplete marks the tray as running with an incomplete config.
// The tray starts in "setup mode" — triggerIngest, drain, and prune are
// all gated, and the icon/menu show a "not configured" state until all
// required fields are set.
func (r *Runner) SetConfigIncomplete(incomplete bool, missing []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configIncomplete = incomplete
	r.missingFields = append([]string(nil), missing...)
	r.configRevision++ // called on startup and on every settings reload
}

// ConfigIncomplete reports whether the tray is currently running with a
// config missing required fields. Mirrors ConfirmDestructive's pattern of
// a locked getter over the field it guards -- triggerIngest, TriggerDrain,
// and TriggerPrune all call this instead of reading r.configIncomplete
// directly, since SetConfigIncomplete can be called concurrently from the
// settings-reload goroutine while any of the three run on their own.
func (r *Runner) ConfigIncomplete() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.configIncomplete
}

// SetConfirmDestructive updates the live confirmDestructive flag so the
// operator's Settings toggle takes effect without a tray restart.
func (r *Runner) SetConfirmDestructive(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.confirmDestructive = v
}

// ConfirmDestructive returns whether destructive menu actions require
// confirmation. Read fresh at each gate site so the Settings toggle
// takes effect immediately.
func (r *Runner) ConfirmDestructive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.confirmDestructive
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

// IngestNowVolumes returns the card volumes the "Ingest now" menu item
// should ingest: every volume currently mounted under the configured
// cardRoots that the detector would report (honouring requireDCIM), minus
// any volume that holds the archive or local edit root. WatchDirs itself
// is the list of PARENT directories (/Volumes, /media/$USER) and must never
// be ingested directly -- that walks every mounted disk, the NAS included.
func (r *Runner) IngestNowVolumes() []string {
	r.mu.Lock()
	cardRoots := append([]string(nil), r.watchDirs...)
	destRoots := []string{r.archiveRoot, r.scratchDir}
	r.mu.Unlock()
	r.detectorMu.Lock()
	requireDCIM := r.detectorRequireDCIM
	r.detectorMu.Unlock()
	kept, excluded := selectCardVolumes(cardRoots, requireDCIM, destRoots...)
	for _, v := range excluded {
		slog.Info("tray: Ingest now is skipping a volume that holds the archive or local edit root",
			"volume", agentlog.Sanitize(v))
	}
	return kept
}

// ingestNowEmptyMessage is the notification "Ingest now" shows when
// IngestNowVolumes found nothing to ingest. With no cardRoots configured
// there is nowhere to look, so "no card found" would mislead -- point the
// operator at the setting instead.
func ingestNowEmptyMessage(cardRoots []string) string {
	if len(cardRoots) == 0 && !removableDriveDetection {
		return "No card folders are configured -- add one under Settings > Watch folders"
	}
	return "No camera card found to ingest"
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
// gate. Manual triggers bypass the confirmation dialog and session skip set.
// When paused (issue #83), triggerIngest returns early without acquiring
// gate: a manual call drops the volume, a detection-driven one is held for
// the next resume (holdPausedInserts).
//
// If the archive destination is probed and found unreachable:
//   - If offline queueing is configured (queueReader != nil), it transparently
//     falls back to IngestCardOffline.
//   - If offline queueing is not configured, it records an error and invokes
//     the registered error notification handler.
func (r *Runner) TriggerIngest(ctx context.Context, cardPath string) IngestSummary {
	return r.triggerIngest(ctx, cardPath, false, false)
}

// ErrGateBusy is TriggerIngestNoWait's answer when something else (another
// ingest, a prune pass, a self-update) holds Runner.gate.
var ErrGateBusy = errors.New("tray: an ingest, prune or update holds the gate")

// TriggerIngestNoWait is TriggerIngest that never waits for Runner.gate: if
// the gate is held it returns at once with Err = ErrGateBusy and ingests
// nothing. The window's ingest-start batch uses it so a prune or update
// that takes the gate between two volumes ends the batch instead of
// leaving it blocked behind the gate, where CancelIngest cannot reach it.
func (r *Runner) TriggerIngestNoWait(ctx context.Context, cardPath string) IngestSummary {
	return r.triggerIngest(ctx, cardPath, false, true)
}

// TriggerDetectedIngest runs one IngestCard pass for a newly detected card
// path. If an IngestGate is configured, it prompts the operator for
// confirmation (or checks the allow-list) before proceeding. Explicitly
// skipped volumes are added to the session skip set and ignored until removed.
func (r *Runner) TriggerDetectedIngest(ctx context.Context, cardPath string) IngestSummary {
	return r.triggerIngest(ctx, cardPath, true, false)
}

// gateBusy is TriggerIngestNoWait's answer when the gate is held. The
// window shows only the last completed ingest, so the operator is told
// with a notification that the card was not ingested.
func (*Runner) gateBusy(cardPath string, notifyErr func(title, msg string)) IngestSummary {
	if notifyErr != nil {
		notifyErr("branchDAM Ingest", fmt.Sprintf("%s was not ingested: another ingest, a prune or an update is running. Start it again once that finishes.", volumeName(cardPath)))
	}
	return IngestSummary{CardPath: cardPath, Err: ErrGateBusy}
}

func (r *Runner) triggerIngest(ctx context.Context, cardPath string, isDetection, noWait bool) IngestSummary {
	acquireGate := func() bool { r.lockGate(); return true }
	if noWait {
		acquireGate = r.tryLockGate
	}
	// A detection that finds the tray paused (including one that raced a
	// re-pause after offerCardsHeldDuringPause checked) is held for the
	// next resume rather than dropped: the detector won't report an
	// unchanged volume again. If the tray resumed in between, nothing is
	// held and the ingest goes ahead.
	if r.paused.Load() && (!isDetection || r.holdPausedInserts(ingest.Diff{Inserted: []string{cardPath}})) {
		slog.Info("tray: ingest paused, skipping", "path", agentlog.Sanitize(cardPath))
		return IngestSummary{CardPath: cardPath}
	}
	if r.ConfigIncomplete() {
		slog.Info("tray: config incomplete, skipping ingest", "path", agentlog.Sanitize(cardPath))
		return IngestSummary{CardPath: cardPath, Err: ErrConfigIncomplete}
	}
	if isDetection {
		r.mu.Lock()
		if r.skipped != nil && r.skipped[cardPath] {
			r.mu.Unlock()
			return IngestSummary{CardPath: cardPath}
		}
		gate := r.ingestGate
		r.mu.Unlock()

		if gate != nil {
			proceed, err := gate.Confirm(ctx, cardPath, volumeName(cardPath))
			if err != nil {
				return IngestSummary{CardPath: cardPath, Err: err}
			}
			if !proceed {
				r.mu.Lock()
				if r.skipped == nil {
					r.skipped = make(map[string]bool)
				}
				r.skipped[cardPath] = true
				r.mu.Unlock()
				return IngestSummary{CardPath: cardPath}
			}
		}
	}

	r.mu.Lock()
	prober := r.archiveProber
	archiveRoot := r.archiveRoot
	hasQueue := r.queueReader != nil
	notifyErr := r.notifyError
	r.mu.Unlock()

	summary := IngestSummary{CardPath: cardPath}
	reachable := prober == nil || prober(ctx, archiveRoot)
	if !reachable && !hasQueue {
		summary.StartedAt = time.Now()
		summary.Elapsed = 0
		msg := "NAS unreachable. Set offline.queueDbPath to enable field ingest"
		summary.Err = errors.New(msg)
		if notifyErr != nil {
			notifyErr("branchDAM Ingest", msg)
		}
		if !acquireGate() {
			// The operator already has the NAS notice: return its
			// reason, not a second, gate-busy one. (r.last is only
			// written under the gate, so it is left as it was.)
			return summary
		}
		r.mu.Lock()
		r.last = &summary
		r.mu.Unlock()
		r.unlockGate()
		return summary
	}
	if !reachable {
		summary.Offline = true
	}

	if !acquireGate() {
		return r.gateBusy(cardPath, notifyErr)
	}
	defer r.unlockGate()
	summary.StartedAt = time.Now()

	r.lastProgress.Store(nil)
	r.setBusy(true, cardPath)
	defer func() {
		r.lastProgress.Store(nil)
		r.setBusy(false, "")
	}()

	// The ingest runs under a stop signal CancelIngest can fire. It only
	// ever takes effect between files (ingest.WithFileBoundaryCancel), so
	// a stop never tears down a DualWrite mid-stream or leaves a copy
	// without its registration -- the same guarantee every action
	// handler's context.WithoutCancel keeps for a disconnected client.
	ingestCtx, stop := ingest.WithFileBoundaryCancel(ctx)
	r.mu.Lock()
	ingester := r.ingester
	r.ingestStop = stop
	r.ingestStopRequested = false
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ingestStop = nil
		r.ingestStopRequested = false
		r.mu.Unlock()
		stop()
	}()
	r.wireProgress(ingester)

	// A stopped walk returns its partial result alongside the error, so
	// the files are counted whether or not err is set (every other error
	// comes back with an empty result).
	if reachable {
		result, err := ingester.IngestCard(ingestCtx, cardPath)
		summary.Elapsed = time.Since(summary.StartedAt)
		if err != nil {
			summary.Err = err
			summary.Canceled = errors.Is(err, ingest.ErrCanceled)
		}
		for _, f := range result.Files {
			switch {
			case f.Err != nil:
				summary.Failed++
			case f.Deferred:
				summary.Deferred++
			case f.Skipped:
				summary.Skipped++
			default:
				summary.Submitted++
			}
		}
	} else {
		result, err := ingester.IngestCardOffline(ingestCtx, cardPath)
		summary.Elapsed = time.Since(summary.StartedAt)
		if err != nil {
			summary.Err = err
			summary.Canceled = errors.Is(err, ingest.ErrCanceled)
		}
		// No Deferred arm: offline ingest never uploads (the archive copy
		// and registration wait in queue.db for the drain), so a metered
		// connection cannot hold anything back here and OfflineFileResult
		// has no Deferred flag.
		for _, f := range result.Files {
			switch f.Outcome() {
			case ingest.OfflineOutcomeFailed:
				summary.Failed++
			case ingest.OfflineOutcomeResumed:
				summary.Resumed++
			case ingest.OfflineOutcomeSkipped:
				summary.Skipped++
			case ingest.OfflineOutcomeQueued:
				summary.Submitted++
			}
		}
	}

	r.mu.Lock()
	r.last = &summary
	notifier := r.notifier
	autoEject := r.autoEject
	ejectFn := r.ejectFn
	cardRoots := append([]string(nil), r.watchDirs...)
	destRoots := []string{r.archiveRoot, r.scratchDir}
	r.mu.Unlock()

	volName := volumeName(cardPath)

	if summary.Deferred > 0 && notifier != nil {
		notifier("branchDAM Agent", fmt.Sprintf("%d file(s) from %s are waiting for an unmetered connection -- card not ejected; re-insert it to finish", summary.Deferred, volName))
	}

	if shouldAutoEject(autoEject, isDetection, cardPath, cardRoots, destRoots, summary) {
		if ejectFn == nil {
			ejectFn = eject.Eject
		}
		if err := ejectFn(cardPath); err != nil {
			slog.Error("failed to eject card", "card", agentlog.Sanitize(cardPath), "err", agentlog.Sanitize(err.Error()))
			if notifier != nil {
				notifier("branchDAM Agent", "Eject failed — please eject manually")
			}
		} else {
			if notifier != nil {
				notifier("branchDAM Agent", fmt.Sprintf("%s ejected — safe to remove", volName))
			}
		}
	} else if notifier != nil && summary.OK() && summary.Submitted > 0 {
		action := "imported"
		if summary.Offline {
			action = "queued offline"
		}
		var msg string
		if summary.Submitted == 1 {
			msg = fmt.Sprintf("1 photo %s from %s", action, volName)
		} else {
			msg = fmt.Sprintf("%d photos %s from %s", summary.Submitted, action, volName)
		}
		notifier("branchDAM Agent", msg)
	}

	return summary
}

// CancelIngest asks the in-flight ingest, if any, to stop before its next
// file and reports whether there was one to stop. The file being copied
// finishes -- copy, verify and registration or queue row -- so nothing is
// left half-done; the summary then reports Canceled, the card is not
// auto-ejected, and a later ingest of the same card resumes where this one
// stopped (already-ingested files are skipped). Deliberately not a pause
// gate: it never touches Paused(), and it does not take gate, so it can
// never block behind (or deadlock with) the ingest it is stopping.
func (r *Runner) CancelIngest() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ingestStop == nil {
		return false
	}
	r.ingestStop()
	r.ingestStopRequested = true
	return true
}

func (r *Runner) setBusy(busy bool, cardPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = busy
	r.busyCard = cardPath
	if busy {
		r.busySince = time.Now()
	} else {
		r.lastProgress.Store(nil)
		// Reset busySince to the zero value when the ingest finishes so
		// Status().BusySince returns a "never" sentinel rather than a
		// stale timestamp from the last completed ingest. The zero
		// time.Time is the same "never" pattern the template uses for
		// LastHandshakeAt below; the status page's "Running… since
		// HH:MM:SS" line keys off busy && !busySince.IsZero() so the
		// reset cleanly hides the indicator when the tray is idle
		// (issue #109 / audit B-17 follow-up).
		r.busySince = time.Time{}
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
	r.gateHeld.Store(true)
	return r.unlockGate, true
}

// GateHeld reports whether the shared ingest/update reservation is held.
// Unlike Busy, this covers the short interval after a self-update acquires
// the gate and before it publishes its downloading phase.
func (r *Runner) GateHeld() bool { return r.gateHeld.Load() }

func (r *Runner) lockGate() {
	r.gate.Lock()
	r.gateHeld.Store(true)
}

// tryLockGate is lockGate without waiting: false when the gate is held.
func (r *Runner) tryLockGate() bool {
	if !r.gate.TryLock() {
		return false
	}
	r.gateHeld.Store(true)
	return true
}

func (r *Runner) unlockGate() {
	// Clear the marker before unlocking. A new owner can only acquire the
	// mutex after this point, so it cannot be overwritten by this release.
	r.gateHeld.Store(false)
	r.gate.Unlock()
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

// SetServerProbe wires (or clears, with a nil probe) the on-demand
// server-reachability check TriggerServerProbe drives. Unlike
// SetQueueDeps, this is called again on every settings reload
// (cmd/branchdam-agent's configSettings.reload) -- see the Runner field
// doc comment above for why a stale client here is a real staleness
// hazard that SetQueueDeps' own drainer/pruner never have to worry about.
func (r *Runner) SetServerProbe(probe ServerProbe) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probe = probe
	if probe == nil {
		// A stale "reachable" (or "unreachable") result must not survive
		// the server fields going from configured to unconfigured --
		// TriggerServerProbe's own doc comment ("a stale reachable
		// surviving a real outage would be actively misleading") applies
		// here too. Without this, an operator who blanks server.baseUrl
		// in Settings still sees the Server card reporting "reachable"
		// from before they cleared it, since TriggerServerProbe's own
		// nil-probe guard returns before ever touching r.lastProbe.
		r.lastProbe = nil
	}
}

// SeedLastHandshakeAt pre-populates the in-memory carry-forward
// state with a prior successful handshake, so a tray that loaded a
// persisted runtime.json at startup can render the status page's
// "last handshake: <since> ago" line before the first in-session
// drain pass runs (issue #149 / audit F-13 cross-session half).
//
// A zero time.Time is treated as the "never" sentinel and is a
// no-op: a fresh install that loaded an empty runtime state file
// (or no file at all) must not pre-populate lastDrain with the
// zero time, which the template's
// {{ if not .Status.LastHandshakeAt.IsZero }} guard already
// suppresses but the test pins as a stronger invariant.
//
// If a real drain pass has already completed in this session
// (r.lastDrain != nil), the seed is silently ignored: a fresh
// in-memory signal from the running tray is always more accurate
// than whatever the runtime.json file held, and overwriting it
// with a stale persisted stamp would briefly regress the status
// page to an older "successful Nh ago" line during the few
// milliseconds between NewRunner returning and Seed being called.
//
// The guard is "r.lastDrain == nil || r.lastDrain.LastHandshakeAt
// is zero" -- not just "r.lastDrain == nil" -- because the drain
// timer can fire between NewRunner returning and Seed being called
// (a 5s drain cadence on an offline server would, with high
// probability, hit this race on a fresh install: NewRunner, then
// SetQueueDeps + go startPeriodic, then the wiring code that calls
// Seed). A failed first-drain pass leaves r.lastDrain non-nil with
// a zero LastHandshakeAt, and keying the guard on the carry-forward
// invariant (Key Invariant 11: "r.lastDrain != nil &&
// !r.lastDrain.LastHandshakeAt.IsZero()") means the seed wins
// whenever the in-memory state does not already have a successful
// stamp. Otherwise a 5s blip on the first post-restart drain pass
// would silently drop the cross-restart signal this whole PR exists
// to defend.
//
// The seeded DrainSummary has HandshakeOK set to false, not true:
// the seeded stamp is from a *prior session's* successful
// handshake, and Status().HandshakeOK is the *current session's*
// "last drain: handshake OK" signal. A pre-seed HandshakeOK=true
// would briefly lie about the current session during the 0-5s
// window before the drain timer's first tick. The first in-session
// drain pass (successful or not) is what flips HandshakeOK to its
// honest current-session value; the seed only contributes the
// cross-session freshness stamp.
func (r *Runner) SeedLastHandshakeAt(t time.Time) {
	if t.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastDrain != nil && !r.lastDrain.LastHandshakeAt.IsZero() {
		return
	}
	r.lastDrain = &DrainSummary{
		At:              t,
		HandshakeOK:     false,
		LastHandshakeAt: t,
	}
}

// SetOnSuccessfulHandshake registers a callback invoked after every
// TriggerDrain pass whose HandshakeOK is true, with the
// just-stamped summary.LastHandshakeAt as its argument. The
// callback may return an error; the error is logged at WARN level
// but does NOT propagate to the drain pass's return value -- a
// failing runtime state save (disk full, permission revocation)
// must never block a drain pass, since the freshness signal the
// state file preserves is non-critical and the in-memory
// carry-forward is the primary correctness layer (issue #149 AC:
// "failing os.WriteFile to the runtime state file should not block
// the drain pass"). A panicking callback is also recovered, for
// the same reason.
//
// Wired by runTrayCmd to internal/runtime.Save; nil disables
// the persistence side of issue #149 (the in-memory carry-forward
// in PR #148 stays in effect regardless, so this is never a
// correctness regression -- only a loss of cross-restart
// persistence).
func (r *Runner) SetOnSuccessfulHandshake(cb func(t time.Time) error) {
	r.onSuccessfulHandshake.Store(&cb)
}

// SetPauseUploadOnMetered sets whether queue drain and streaming upload
// operations should be deferred when connected to a metered network.
func (r *Runner) SetPauseUploadOnMetered(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pauseUploadOnMetered = v
}

// PauseUploadOnMetered reports whether pause-on-metered is currently enabled.
func (r *Runner) PauseUploadOnMetered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pauseUploadOnMetered
}

// SetIsMeteredFunc overrides the network meteredness probe function (useful for tests).
func (r *Runner) SetIsMeteredFunc(fn func() (bool, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.isMeteredFn = fn
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
// from ever piling up. ran=false covers "not configured", "already
// running", "ingest paused", and "config incomplete" (the dummy client
// used while required fields are missing -- see ConfigIncomplete); the
// caller (a timer tick or a menu click) treats all four the same way:
// nothing to show beyond what Status() already reports.
func (r *Runner) TriggerDrain(ctx context.Context) (summary DrainSummary, ran bool) {
	if r.paused.Load() {
		return DrainSummary{}, false
	}
	if r.ConfigIncomplete() {
		return DrainSummary{}, false
	}
	if !r.drainMu.TryLock() {
		return DrainSummary{}, false
	}
	defer r.drainMu.Unlock()

	r.mu.Lock()
	drainer := r.drainer
	pauseMetered := r.pauseUploadOnMetered
	isMetered := r.isMeteredFn
	if isMetered == nil {
		isMetered = netgate.IsMetered
	}
	r.mu.Unlock()

	if drainer == nil {
		return DrainSummary{}, false
	}

	if pauseMetered {
		if metered, mErr := isMetered(); metered || mErr != nil {
			if mErr != nil {
				slog.Debug("metered probe failed, treating as metered (fail-closed)", "err", mErr)
			}
			slog.Info("upload skipped on metered connection")
			return DrainSummary{}, false
		}
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
	// Preserve the prior successful LastHandshakeAt across failed
	// drain passes: the Drainer leaves summary.LastHandshakeAt zero
	// when HandshakeOK is false, but overwriting r.lastDrain with
	// that zero would erase "successful 4h ago" the moment a 5s
	// tick's handshake blip happens, which is exactly the freshness
	// signal the status page's "last handshake" line is meant to
	// preserve. Key off the preserved stamp's non-zero state, not
	// the prior pass's HandshakeOK: at a 5s drain cadence any outage
	// >~10s would otherwise wipe the signal after a single recovery
	// blip already preserved it once (Hermes review on PR #148:
	// keying on r.lastDrain.HandshakeOK only survives ONE
	// consecutive failure, because after the carry-forward the prior
	// summary's HandshakeOK is false, so a second failure drops the
	// stamp). Initial failures (no prior successful stamp) stay at
	// zero -- the "never completed a successful handshake" sentinel
	// per DrainSummary.LastHandshakeAt's doc.
	if summary.LastHandshakeAt.IsZero() && r.lastDrain != nil && !r.lastDrain.LastHandshakeAt.IsZero() {
		summary.LastHandshakeAt = r.lastDrain.LastHandshakeAt
	}
	r.lastDrain = &summary
	r.mu.Unlock()

	// Cross-session persistence hook (issue #149): on every
	// successful drain pass, ask the registered callback to write
	// the just-stamped LastHandshakeAt to the runtime state file.
	// The callback is allowed to fail (logged via slog from the
	// call site) and panic (recovered here): a misbehaving save
	// must not corrupt the drain pass's own return value, which
	// is the trigger timer's only signal that the pass completed.
	// We deliberately do this AFTER r.mu.Unlock() so a slow save
	// (e.g. a slow SMB share holding runtime.json) does not block
	// the next drain pass from acquiring the mutex.
	//
	// The callback is read via an atomic.Pointer (not
	// r.onSuccessfulHandshake under r.mu) so a future
	// SetOnSuccessfulHandshake swap from a settings-reload hook
	// does not race the read here: at worst, a swap that lands
	// between the Load and the call uses the *old* callback for
	// this one drain pass and the *new* one from the next pass
	// onward -- the freshness signal lands at one of the two
	// configured destinations, never lost, which is the only
	// invariant the persistence hook actually owns.
	cbPtr := r.onSuccessfulHandshake.Load()
	if summary.HandshakeOK && cbPtr != nil {
		cb := *cbPtr
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("onSuccessfulHandshake callback panicked; runtime state will not be persisted this pass", "panic", rec)
				}
			}()
			if err := cb(summary.LastHandshakeAt); err != nil {
				slog.Warn("runtime state save failed; the last-handshake signal will not survive the next restart", "err", err)
			}
		}()
	}

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
	if r.paused.Load() {
		return PruneSummary{}, false
	}
	if r.ConfigIncomplete() {
		return PruneSummary{}, false
	}
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

	pruneStart := time.Now()
	summary, err := pruner.Prune(ctx)
	summary.At = time.Now()
	summary.Duration = summary.At.Sub(pruneStart)
	if err != nil {
		summary.Err = err
	}

	r.mu.Lock()
	r.lastPrune = &summary
	r.mu.Unlock()

	return summary, true
}

// TriggerServerProbe runs one ServerProbe.Probe call, if one is wired and
// no other probe is already running. Deliberately mirrors TriggerPrune's
// shape (its own dedicated probeMu, not Runner.gate -- a connectivity
// check has nothing to do with an in-flight ingest or self-update) with
// two differences:
//
//  1. NOT gated on r.paused.Load() or r.ConfigIncomplete(). Pause exists to
//     suspend automatic card detection and queue draining (shoot-mode,
//     issue #83) -- it says nothing about whether an operator should be
//     able to click "Test connection" mid-shoot. ConfigIncomplete is set
//     the moment ingest.localEditRoot/archiveRoot/pathMappings are
//     missing, but confirming the server half (baseUrl/apiKey/agentId)
//     already works is exactly the diagnostic an operator needs *while*
//     still filling in the storage half -- gating this the same way
//     TriggerDrain/TriggerPrune are would reproduce this very bug for a
//     second, unrelated pill.
//  2. No carry-forward of a prior result on failure (unlike TriggerDrain's
//     LastHandshakeAt preservation): a probe is a point-in-time check,
//     and a stale "reachable" surviving a real outage would be actively
//     misleading for what this exists to answer.
func (r *Runner) TriggerServerProbe(ctx context.Context) (result ProbeResult, ran bool) {
	if !r.probeMu.TryLock() {
		return ProbeResult{}, false
	}
	defer r.probeMu.Unlock()

	r.mu.Lock()
	probe := r.probe
	r.mu.Unlock()

	if probe == nil {
		return ProbeResult{}, false
	}

	r.mu.Lock()
	r.inFlightProbe = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlightProbe = false
		r.mu.Unlock()
	}()

	version, err := probe.Probe(ctx)
	result = ProbeResult{At: time.Now(), Version: version, Err: err, OK: err == nil}

	r.mu.Lock()
	r.lastProbe = &result
	r.mu.Unlock()

	return result, true
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

	// Resolve's confirmed scope is saved inside the syncer after the
	// server transaction succeeds. The runner stores only this summary;
	// it never guesses membership state from a tray-level callback.

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
// completes; RefreshHookState keeps it fresh after a settings reload that
// changes a field the installer reads (issue #154); nothing else ever
// needs to call this again.
func (r *Runner) SetHookState(id HookID, st HookState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamped := st
	r.hookState[id] = &stamped
}

// RefreshHookState overwrites the cached state for id with a fresh
// snapshot the caller has already computed (typically a
// resolvehook.Detect result against a freshly reloaded
// integrations.resolve.scriptsDir). Mirrors SetHookState's own contract:
// direct cache writer, never calls the registered installer, never
// recomputes from Status() or a refresh tick -- the "compute" side
// belongs to the cmd/branchdam-agent reload path, which has the new
// config in hand and can decide whether the relevant field actually
// changed (issue #154, audit F-17).
//
// The write races with TriggerHookInstall's own post-completion cache
// write; whichever call grabs r.mu last wins. A slow install that
// completes AFTER a refresh will still surface its own result, so a user
// who clicks "Install" right after editing a setting sees the install
// outcome rather than the pre-install refresh snapshot -- pinned by
// TestRefreshHookStateAfterInstallWins.
func (r *Runner) RefreshHookState(id HookID, st HookState) {
	r.SetHookState(id, st)
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
// pattern as cmd/branchdam-agent/settings.go's openWithDefaultApp: Reveal
// never mutates hook state, so unlike TriggerHookInstall it needs neither
// hookInFlight tracking nor a SetHookState cache update, and a caller is
// free to discard the error the same way that helper's own callers do.
func (r *Runner) RevealHook(id HookID) error {
	r.mu.Lock()
	installer := r.hookInstallers[id]
	r.mu.Unlock()
	if installer == nil {
		return fmt.Errorf("tray: no hook installer registered for %q", id)
	}
	return installer.Reveal()
}

// SetIngestGate wires the gate used to confirm card imports before proceeding (issue #79).
// A nil gate (the default) means always proceed -- preserving existing test and headless
// behavior.
func (r *Runner) SetIngestGate(g IngestGate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingestGate = g
}

// IngestGate returns the registered IngestGate, or nil if none was set.
func (r *Runner) IngestGate() IngestGate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ingestGate
}

// SetNotifier registers a callback for emitting user-facing OS desktop notifications (issue #79).
func (r *Runner) SetNotifier(fn func(title, message string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifier = fn
}

// SetAutoEject sets whether cards should be automatically unmounted/ejected
// after a successful verified ingest (issue #87).
func (r *Runner) SetAutoEject(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.autoEject = v
}

// AutoEject reports whether auto-eject is enabled.
func (r *Runner) AutoEject() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoEject
}

// SetEjectFunc overrides the eject implementation called when autoEject is enabled (used for tests).
func (r *Runner) SetEjectFunc(fn func(mountPath string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn == nil {
		fn = eject.Eject
	}
	r.ejectFn = fn
}

// ForgetSkipped clears a volume path from the session-scoped skip set
// when the volume is unmounted / removed (issue #79).
func (r *Runner) ForgetSkipped(volumePath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.skipped, volumePath)
}

// IsSkipped reports whether volumePath is in the session-scoped skip set.
func (r *Runner) IsSkipped(volumePath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skipped[volumePath]
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
// ingest.Detector. If d <= 0, ingest.DefaultPollInterval is used. It does
// not touch a running detector itself: the next Reconfigure that is
// applied restarts the detector when its interval differs (a settings
// reload sets the interval and then calls Reconfigure), so the change is
// applied under the same never-under-a-card-mid-copy rule as the engine.
func (r *Runner) SetDetectorInterval(d time.Duration) {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()
	if d <= 0 {
		d = ingest.DefaultPollInterval
	}
	r.detectorInterval = d
}

// SetDetectorRequireDCIM sets whether a volume needs a DCIM/ subdirectory
// to count as a card. IngestNowVolumes and the resume offer use the new
// value at once. Like SetDetectorInterval, it does not touch a running
// detector itself: the next Reconfigure that is applied restarts the
// detector when its DCIM requirement differs (a settings reload sets it
// and then calls Reconfigure). Restarting it here would cancel a
// detection-triggered ingest, which runs on the detector's context.
func (r *Runner) SetDetectorRequireDCIM(v bool) {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()
	r.detectorRequireDCIM = v
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

	// No roots stops the detector -- except where removable drives are
	// detected on their own (Windows), which needs no cardRoots at all.
	if len(roots) == 0 && !removableDriveDetection {
		return
	}

	// Same volume selection as IngestNowVolumes: a disk holding the archive
	// or local edit root is never offered as a card, whichever path asks.
	requireDCIM := r.detectorRequireDCIM

	dctx, cancel := context.WithCancel(baseCtx)
	r.detectorCancel = cancel
	r.detectorRunningInterval = interval
	r.detectorRunningRequireDCIM = requireDCIM
	done := make(chan struct{})
	r.detectorDone = done
	detectorRoots := append([]string(nil), roots...)
	r.mu.Lock()
	known := make([]string, 0, len(r.detectedVolumes))
	for v := range r.detectedVolumes {
		known = append(known, v)
	}
	r.mu.Unlock()
	detector := &ingest.Detector{
		Known: known,
		List: func() ([]string, error) {
			r.mu.Lock()
			destRoots := []string{r.archiveRoot, r.scratchDir}
			r.mu.Unlock()
			kept, _ := selectCardVolumes(detectorRoots, requireDCIM, destRoots...)
			return kept, nil
		},
		Interval: interval,
	}
	go func() {
		defer close(done)
		err := detector.Watch(dctx, func(diff ingest.Diff) {
			r.recordDetected(diff)
			for _, path := range diff.Removed {
				r.ForgetSkipped(path)
			}
			if r.holdPausedInserts(diff) {
				return
			}
			for _, path := range diff.Inserted {
				r.TriggerDetectedIngest(dctx, path)
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

// detectorStale reports whether a detector is running with a poll
// interval or DCIM requirement other than the one SetDetectorInterval /
// SetDetectorRequireDCIM last set.
func (r *Runner) detectorStale() bool {
	r.detectorMu.Lock()
	defer r.detectorMu.Unlock()
	want := r.detectorInterval
	if want <= 0 {
		want = ingest.DefaultPollInterval
	}
	return r.detectorCancel != nil &&
		(r.detectorRunningInterval != want || r.detectorRunningRequireDCIM != r.detectorRequireDCIM)
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

// pendingReconfig is one Reconfigure call's arguments.
type pendingReconfig struct {
	ingester   Ingester
	watchDirs  []string
	scratchDir string
}

// ingesterCloser is implemented by an Ingester that owns resources -- the
// *ingest.Engine's pooled exiftool processes -- to release once it has
// been replaced.
type ingesterCloser interface{ Close() }

// Reconfigure swaps in a freshly built Ingester plus the watch/scratch
// description it should report going forward -- the guarded-rebuild
// mechanism issue #31's settings menu applies every hot-reloadable config
// change through, rather than giving each individual field (server URL,
// API key, archive/local roots, naming template, requireUnbuffered) its
// own bespoke hot-patch path. The swap happens under TriggerIngest's own
// gate, so a config reload can never race a card mid-copy.
//
// Reconfigure never waits for that gate. When it is free the swap happens
// before Reconfigure returns (applied == true). When an ingest (or a
// self-update) holds it, the call is recorded and applied by a background
// goroutine the moment the gate frees up, and Reconfigure returns false
// at once: a settings save during a long card copy must not hang the
// caller -- the Settings window's request would time out and report the
// agent as not running, though the change was saved. Only the newest
// pending call is applied; an older one it superseded never is, and its
// Ingester is Closed as soon as it is superseded (it was never installed,
// so no ingest can be using it). ReconfigurePending reports whether one
// is still waiting.
//
// The replaced Ingester is Closed (when it implements Close) right after
// the swap, still under the gate, so the old engine's exiftool pool
// doesn't outlive it -- no ingest can be using it at that point.
//
// When watchDirs differs from the current roots, or the running detector
// polls at an interval or DCIM requirement other than SetDetectorInterval's
// / SetDetectorRequireDCIM's latest (a reload that changed
// ingest.pollIntervalSecs or ingest.requireDCIM), the detector is
// restarted (ReconfigureDetector, issue #78) AFTER releasing gate, so
// waiting on the previous detector goroutine to exit cannot deadlock
// against an in-flight TriggerIngest call. Because the restart rides the
// applied Reconfigure, it never happens while an ingest holds the gate:
// restarting cancels the detector's context, which a detection-triggered
// ingest runs on.
//
// One field is deliberately NOT reconfigurable this way and is the
// caller's responsibility to treat as restart-required instead:
// tray.statusAddr (the embedded HTTP server's Listen() call already
// happened and is this tray's single-instance guard -- there's nothing to
// swap it into).
func (r *Runner) Reconfigure(ingester Ingester, watchDirs []string, scratchDir string) (applied bool) {
	r.reconfigMu.Lock()
	displaced := r.reconfigPending
	r.reconfigPending = &pendingReconfig{
		ingester:   ingester,
		watchDirs:  append([]string(nil), watchDirs...),
		scratchDir: scratchDir,
	}
	r.reconfigMu.Unlock()
	if displaced != nil {
		r.closeDisplacedIngester(displaced.ingester, ingester)
	}

	if r.gate.TryLock() {
		r.gateHeld.Store(true)
		r.applyPendingReconfigAndUnlock()
		return true
	}

	r.reconfigMu.Lock()
	startWaiter := !r.reconfigWaiting
	r.reconfigWaiting = true
	r.reconfigMu.Unlock()
	if startWaiter {
		go func() {
			r.lockGate()
			// Cleared before taking the pending call: a Reconfigure that
			// lands after this point either is taken below or, seeing no
			// waiter, starts its own.
			r.reconfigMu.Lock()
			r.reconfigWaiting = false
			r.reconfigMu.Unlock()
			r.applyPendingReconfigAndUnlock()
		}()
	}
	return false
}

// ReconfigurePending reports whether a Reconfigure call is still waiting
// for an in-flight ingest (or self-update) to release the gate.
func (r *Runner) ReconfigurePending() bool {
	r.reconfigMu.Lock()
	defer r.reconfigMu.Unlock()
	return r.reconfigPending != nil
}

// applyPendingReconfigAndUnlock applies the newest pending Reconfigure
// call, if any. Called with gate held; releases it before restarting the
// detector.
func (r *Runner) applyPendingReconfigAndUnlock() {
	r.reconfigMu.Lock()
	p := r.reconfigPending
	r.reconfigPending = nil
	r.reconfigMu.Unlock()
	if p == nil {
		r.unlockGate()
		return
	}

	r.mu.Lock()
	old := r.ingester
	rootsChanged := !slices.Equal(r.watchDirs, p.watchDirs)
	r.ingester = p.ingester
	r.wireProgress(p.ingester)
	r.scratchDir = p.scratchDir
	if rootsChanged {
		r.watchDirs = p.watchDirs
	}
	r.mu.Unlock()

	if c, ok := old.(ingesterCloser); ok && old != p.ingester {
		c.Close()
	}
	r.unlockGate()

	if rootsChanged || r.detectorStale() {
		r.ReconfigureDetector(r.BaseContext(), p.watchDirs)
	}
}

// closeDisplacedIngester Closes the Ingester of a pending Reconfigure call
// that a newer one superseded before it was applied. Never installed, so
// nothing can be ingesting with it -- unless it is the one now current
// or the one replacing it (the same engine passed twice), which are left
// open.
func (r *Runner) closeDisplacedIngester(displaced, replacement Ingester) {
	if displaced == nil || displaced == replacement {
		return
	}
	r.mu.Lock()
	current := r.ingester
	r.mu.Unlock()
	if displaced == current {
		return
	}
	if c, ok := displaced.(ingesterCloser); ok {
		c.Close()
	}
}

// CloseIngester releases the current Ingester's resources (see
// ingesterCloser) -- for process shutdown. It does not wait for the gate:
// shutdown must not hang behind a card copy, and an ingest still running
// when its engine's exiftool pool closes falls back to one-shot exiftool
// forks (internal/exiftool.Pool.Execute) rather than failing.
func (r *Runner) CloseIngester() {
	r.mu.Lock()
	ing := r.ingester
	r.mu.Unlock()
	if c, ok := ing.(ingesterCloser); ok {
		c.Close()
	}
}

// wireProgress connects the Runner's progress callback to the underlying
// Engine if ingester is an *ingest.Engine.
func (r *Runner) wireProgress(ingester Ingester) {
	if eng, ok := ingester.(*ingest.Engine); ok && eng != nil {
		eng.Progress = func(e ingest.ProgressEvent) {
			ev := e
			r.lastProgress.Store(&ev)
		}
	}
}

// SetProgress records an ingest progress event (used by tests and progress
// observers).
func (r *Runner) SetProgress(ev *ingest.ProgressEvent) {
	r.lastProgress.Store(ev)
}

// statusQueueReadTimeout bounds Status()'s QueueReader.Counts call -- see
// that call site's comment for why this exists.
const statusQueueReadTimeout = 5 * time.Second

// scratchUsageTTL bounds how often Status() actually re-stats the working
// copy volume -- free space doesn't change fast enough to need a fresh
// syscall on every 5s poll tick, and this is purely a "don't hammer the
// filesystem" cap, not a correctness requirement (scratchUsageSnapshot's
// own dir-mismatch check below is what guarantees correctness across a
// Reconfigure, independent of this TTL).
const scratchUsageTTL = 30 * time.Second

// diskUsageFn is a test seam for diskUsage (the real per-OS syscall, see
// diskusage_{linux,darwin,windows}.go): a real statfs/GetDiskFreeSpaceEx
// call can't be made to hang or fail on demand from a test, and
// TestScratchUsageHangOnAbandonedDirDoesNotBlockNewDir needs exactly that
// to exercise refreshScratchUsage's abandoned-dir guard. Atomic rather
// than a plain var because refreshScratchUsage reads it on a goroutine of
// its own, which can outlive the test that started it: a later test's
// stub swap must not race that read. nil means diskUsage. Read it with
// loadDiskUsageFunc and swap it only with setDiskUsageFunc.
var diskUsageFn atomic.Pointer[func(dir string) (free, total uint64, err error)]

// loadDiskUsageFunc returns the current disk-usage implementation.
func loadDiskUsageFunc() func(dir string) (free, total uint64, err error) {
	if fn := diskUsageFn.Load(); fn != nil {
		return *fn
	}
	return diskUsage
}

// setDiskUsageFunc installs fn as the disk-usage implementation and
// returns a func that restores the previous one -- for tests, as
// t.Cleanup(setDiskUsageFunc(stub)).
func setDiskUsageFunc(fn func(dir string) (free, total uint64, err error)) (restore func()) {
	prev := diskUsageFn.Swap(&fn)
	return func() { diskUsageFn.Store(prev) }
}

// ScratchUsage reports free/total disk space for the volume backing
// ingest.localEditRoot (issue #274 -- replaces the old ScratchNote
// placeholder field, which only ever said "usage tracking not yet
// implemented"). Configured is false when localEditRoot itself is unset
// -- there is nothing to stat. Configured true with Err set (FreeBytes/
// TotalBytes left at their zero value) means localEditRoot IS set but the
// OS-level stat call failed (permission error, an unmounted network
// share): deliberately distinct from "not configured" so a renderer can
// show "usage unavailable: <err>" instead of a misleading "0 B free".
// FreeBytes/TotalBytes carry no `omitempty`: a nearly-full volume
// legitimately reports 0 free, and omitempty would silently hide that
// exact reading -- the one an operator most needs to see.
// Untagged, not camelCase-tagged: QueueStatus (this same file's sibling
// nested status type -- see its own doc comment and MarshalJSON) is
// untagged too, defaulting to PascalCase field names, and app.js's
// renderOverview already reads su.Configured/su.FreeBytes/su.TotalBytes/
// su.Err to match. An earlier revision of this type carried explicit
// lowercase `json:"configured"`-style tags, which silently broke that
// convention: encoding/json emitted "configured" while app.js kept
// reading "Configured", so the Overview panel's "Working copy" tile could
// never see real data -- always undefined, always the neutral
// "not configured" pill, in the shipped app, not just this test file
// (caught in review, not by any automated test, since nothing here
// round-trips through actual JSON).
type ScratchUsage struct {
	Configured bool
	FreeBytes  uint64
	TotalBytes uint64
	Err        string `json:",omitempty"`
}

// scratchUsageSnapshot returns the current cached disk-usage reading for
// dir, kicking off a background refresh when the cache is missing, stale,
// or for a different path than dir -- see scratchUsageMu's own doc comment
// on Runner for why this must never block on the OS-level stat itself.
func (r *Runner) scratchUsageSnapshot(dir string) ScratchUsage {
	if dir == "" {
		r.scratchUsageMu.Lock()
		r.scratchUsageDir = ""
		r.scratchUsage = ScratchUsage{}
		r.scratchUsageAt = time.Time{}
		// Also abandon any refresh still in flight for whatever the OLD
		// (non-empty) scratchDir was -- without this, that goroutine's
		// eventual completion still finds scratchUsageInFlightDir naming
		// its own dir (this branch never touched it) and writes the OLD
		// path's free/total bytes back into the cache one Status() cycle
		// after localEditRoot was cleared, resurrecting exactly the
		// stale reading the dir-mismatch check below exists to prevent
		// (Hermes review finding on this PR).
		r.scratchUsageInFlightDir = ""
		r.scratchUsageMu.Unlock()
		return ScratchUsage{}
	}

	r.scratchUsageMu.Lock()
	sameDir := r.scratchUsageDir == dir
	fresh := sameDir && time.Since(r.scratchUsageAt) < scratchUsageTTL
	current := r.scratchUsage
	// Keyed by dir (scratchUsageInFlightDir), not a bare bool: see that
	// field's own doc comment above for why a refresh hung on some OTHER
	// path must never block one for THIS path. Only one read of
	// r.scratchUsageInFlightDir -- the assignment below doesn't need a
	// second one, since nothing else can change it while this lock is
	// held.
	needRefresh := !fresh && r.scratchUsageInFlightDir != dir
	if needRefresh {
		r.scratchUsageInFlightDir = dir
	}
	r.scratchUsageMu.Unlock()

	if needRefresh {
		go r.refreshScratchUsage(dir)
	}
	if !sameDir {
		// Never show a stale reading for a path this isn't anymore: a
		// Reconfigure to a new localEditRoot must not relabel the OLD
		// path's free/total bytes as the new one's, even within the TTL
		// window above. Configured stays true (a probe is in flight) with
		// no numbers yet, rather than falling back to "not configured".
		return ScratchUsage{Configured: true}
	}
	return current
}

// refreshScratchUsage performs the actual OS-level stat call for dir and
// stores the result -- always called in its own goroutine
// (scratchUsageSnapshot is the only caller). A hung statfs/
// GetDiskFreeSpaceEx blocks nothing but this one goroutine; if the runner
// moves on to a different dir (a Reconfigure) before this call returns,
// the eventual result is discarded rather than clobbering whatever the
// NEW dir's own refresh already wrote -- see the scratchUsageInFlightDir
// check below.
func (r *Runner) refreshScratchUsage(dir string) {
	free, total, err := loadDiskUsageFunc()(dir)
	usage := ScratchUsage{Configured: true}
	if err != nil {
		usage.Err = err.Error()
	} else {
		usage.FreeBytes = free
		usage.TotalBytes = total
	}

	r.scratchUsageMu.Lock()
	defer r.scratchUsageMu.Unlock()
	if r.scratchUsageInFlightDir != dir {
		// Abandoned: something else (another Reconfigure, possibly more
		// than one) took over the in-flight slot while this call --
		// potentially hung for a long time on an unreachable dir -- was
		// still running. This result is stale and no longer relevant;
		// writing it now would clobber the CURRENT dir's own cached
		// reading with data for a path nothing points at anymore.
		return
	}
	r.scratchUsageDir = dir
	r.scratchUsage = usage
	r.scratchUsageAt = time.Now()
	r.scratchUsageInFlightDir = ""
}

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
	configIncomplete := r.configIncomplete
	missingFields := append([]string(nil), r.missingFields...)
	configRevision := r.configRevision
	lastProbe := r.lastProbe
	inFlightProbe := r.inFlightProbe
	pauseUploadOnMetered := r.pauseUploadOnMetered
	cancelRequested := r.ingestStopRequested

	// Built entirely under r.mu, not after unlocking: r.syncers/r.lastSync
	// are read map entries here, not copied whole-map references, so a
	// concurrent TriggerSync write (a single map-key assignment, not a
	// map replace) could otherwise race this read -- see TriggerSync's
	// own r.lastSync[id] = &stamped assignment.
	integrations := make([]IntegrationStatus, 0, len(Integrations()))
	for _, d := range Integrations() {
		st := IntegrationStatus{ID: d.ID, Title: d.Title, Registered: r.syncers[d.ID] != nil}
		if ls, ok := r.lastSync[d.ID]; ok {
			st.LastSync = ls
		}
		integrations = append(integrations, st)
	}

	// Built under the same lock, same reasoning as integrations above:
	// r.hookState is read map entries here, not a copied whole-map
	// reference, so a concurrent TriggerHookInstall write could otherwise
	// race this read. Ranges over HookDescriptors() rather than Hooks()
	// (which the menu-building code used to prefer, back when the ID was
	// all a tray menu item needed) so Title is available to populate
	// HookStatus.Title -- same ID set and order, just with titles attached.
	hooks := make([]HookStatus, 0, len(HookDescriptors()))
	for _, d := range HookDescriptors() {
		id := d.ID
		hs := HookStatus{ID: id, Title: d.Title, Registered: r.hookInstallers[id] != nil}
		if s, ok := r.hookState[id]; ok {
			hs.State = s
		}
		hooks = append(hooks, hs)
	}
	r.mu.Unlock()

	var prog *ingest.ProgressEvent
	if busy {
		prog = r.lastProgress.Load()
	}

	scratchUsage := r.scratchUsageSnapshot(scratchDir)

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

	var lastHandshakeAt time.Time
	if lastDrain != nil {
		lastHandshakeAt = lastDrain.LastHandshakeAt
	}

	return Status{
		WatchDirs:        watchDirs,
		ScratchUsage:     scratchUsage,
		QueueStatus:      qs,
		LastIngest:       last,
		SelfUpdate:       selfUpdate,
		Paused:           r.paused.Load(),
		Busy:             busy,
		BusyCard:         busyCard,
		BusySince:        busySince,
		IngestProgress:   prog,
		HandshakeOK:      lastDrain != nil && lastDrain.HandshakeOK,
		LastHandshakeAt:  lastHandshakeAt,
		HasDrained:       lastDrain != nil,
		InFlightDrain:    inFlightDrain,
		InFlightPrune:    inFlightPrune,
		Integrations:     integrations,
		Hooks:            hooks,
		ConfigIncomplete: configIncomplete,
		MissingFields:    missingFields,
		ConfigRevision:   configRevision,
		ServerProbe:      lastProbe,
		InFlightProbe:    inFlightProbe,

		UploadPausedMetered:   r.meteredUploadPaused(pauseUploadOnMetered),
		IngestCancelRequested: busy && cancelRequested,
	}
}
