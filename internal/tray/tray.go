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

// Status is the full snapshot the embedded status page and the tray menu
// both render from. QueueStatus is a stub -- deliberately literal text, not
// a fabricated number, per the issue's explicit instruction not to show
// fake numbers.
type Status struct {
	WatchDirs      []string
	ScratchNote    string
	QueueStatus    string
	LastIngest     *IngestSummary
	SelfUpdateNote string
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
	Ingester   Ingester
	WatchDirs  []string
	ScratchDir string // LocalEditRoot -- described, not measured; see Status().

	mu   sync.Mutex
	last *IngestSummary
}

// NewRunner builds a Runner over ingester, describing watchDirs (typically
// the configured ingest.cardRoots) and scratchDir (ingest.localEditRoot)
// in the status snapshot.
func NewRunner(ingester Ingester, watchDirs []string, scratchDir string) *Runner {
	return &Runner{Ingester: ingester, WatchDirs: watchDirs, ScratchDir: scratchDir}
}

// TriggerIngest runs one IngestCard pass over cardPath through the same
// Engine the headless `ingest` subcommand uses, records the outcome, and
// returns it. Safe to call from a menu-click handler or from the
// card-detection watch loop -- both paths in run_supported.go go through
// this single method so there is exactly one place that talks to the
// ingest core.
func (r *Runner) TriggerIngest(ctx context.Context, cardPath string) IngestSummary {
	summary := IngestSummary{CardPath: cardPath, StartedAt: time.Now()}

	result, err := r.Ingester.IngestCard(ctx, cardPath)
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

// Status returns a snapshot of the current state for the status page and
// the tray tooltip. selfUpdateNote is passed in rather than computed here
// -- self-update's own check is async and gated by config the caller
// already has (internal/selfupdate), so Runner stays a pure ingest-facing
// type rather than reaching into an unrelated subsystem.
func (r *Runner) Status(selfUpdateNote string) Status {
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()

	scratchNote := "not configured"
	if r.ScratchDir != "" {
		scratchNote = fmt.Sprintf("%s (usage tracking not yet implemented)", r.ScratchDir)
	}

	return Status{
		WatchDirs:      append([]string(nil), r.WatchDirs...),
		ScratchNote:    scratchNote,
		QueueStatus:    QueueStatusStub,
		LastIngest:     last,
		SelfUpdateNote: selfUpdateNote,
	}
}
