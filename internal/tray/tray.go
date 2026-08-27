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
	"fmt"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// Ingester is the subset of *ingest.Engine's surface Runner needs, so tests
// can substitute a fake without touching a real card or a real branchDAM
// server -- same pattern as internal/ingest.Engine's own nodeCreator
// interface and cmd/branchdam-agent/preflight.go's helloCaller.
type Ingester interface {
	IngestCard(ctx context.Context, cardRoot string) (ingest.CardResult, error)
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
// for an update to be installed. Untagged (unlike Run itself) so both
// run_supported.go and run_unsupported.go's stub can share the type.
type Outcome struct {
	RestartRequested bool
	AppliedVersion   string
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
// both render from. QueueStatus is a stub -- deliberately literal text, not
// a fabricated number, per the issue's explicit instruction not to show
// fake numbers.
type Status struct {
	WatchDirs   []string
	ScratchNote string
	QueueStatus string
	LastIngest  *IngestSummary
	SelfUpdate  UpdateStatus
	// Busy and BusyCard reflect Runner.Busy() -- shown on the status page
	// and used by the tray menu to disable "Install and restart" while an
	// ingest is running (see Runner.TryLockIdle for the actual gate this
	// only mirrors for display).
	Busy     bool
	BusyCard string
}

// QueueStatusStub is Status.QueueStatus's placeholder. M2's offline queue
// (issue #4, `internal/queue` + `queue.db`) landed concurrently with this
// PR (a separate worktree, per this repo's issue #3/#4 split) and is not
// wired into the tray/status-page display yet -- that's follow-on work
// (querying `queue.db` for a real depth/backlog readout), not something
// this package should fabricate a number for in the meantime.
const QueueStatusStub = "offline queue exists (M2) but is not yet wired into the tray status page"

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
}

// NewRunner builds a Runner over ingester, describing watchDirs (typically
// the configured ingest.cardRoots) and scratchDir (ingest.localEditRoot)
// in the status snapshot.
func NewRunner(ingester Ingester, watchDirs []string, scratchDir string) *Runner {
	return &Runner{ingester: ingester, watchDirs: watchDirs, scratchDir: scratchDir}
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
func (r *Runner) TriggerIngest(ctx context.Context, cardPath string) IngestSummary {
	r.gate.Lock()
	defer r.gate.Unlock()

	r.setBusy(true, cardPath)
	defer r.setBusy(false, "")

	r.mu.Lock()
	ingester := r.ingester
	r.mu.Unlock()

	summary := IngestSummary{CardPath: cardPath, StartedAt: time.Now()}

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

// Reconfigure swaps in a freshly built Ingester plus the watch/scratch
// description it should report going forward -- the guarded-rebuild
// mechanism issue #31's settings menu applies every hot-reloadable config
// change through, rather than giving each individual field (server URL,
// API key, archive/local roots, naming template, requireUnbuffered) its
// own bespoke hot-patch path. Blocks until no ingest is in flight (reuses
// TriggerIngest's own gate), so a config reload can never race a card
// mid-copy the way swapping these fields without synchronization would.
//
// Two fields are deliberately NOT reconfigurable this way and are the
// caller's responsibility to treat as restart-required instead:
// tray.statusAddr (the embedded HTTP server's Listen() call already
// happened and is this tray's single-instance guard -- there's nothing to
// swap it into) and ingest.cardRoots (internal/ingest.Detector's Watch
// call is a one-shot goroutine over the roots it started with, not
// restartable from inside a running select loop).
func (r *Runner) Reconfigure(ingester Ingester, watchDirs []string, scratchDir string) {
	r.gate.Lock()
	defer r.gate.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.ingester = ingester
	r.watchDirs = append([]string(nil), watchDirs...)
	r.scratchDir = scratchDir
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
	watchDirs := append([]string(nil), r.watchDirs...)
	scratchDir := r.scratchDir
	r.mu.Unlock()

	scratchNote := "not configured"
	if scratchDir != "" {
		scratchNote = fmt.Sprintf("%s (usage tracking not yet implemented)", scratchDir)
	}

	return Status{
		WatchDirs:   watchDirs,
		ScratchNote: scratchNote,
		QueueStatus: QueueStatusStub,
		LastIngest:  last,
		SelfUpdate:  selfUpdate,
		Busy:        busy,
		BusyCard:    busyCard,
	}
}
