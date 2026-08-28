package tray

import (
	"context"
	"time"
)

// QueueCounts is the tray-local mirror of internal/queue.Counts --
// duplicated rather than imported so internal/tray keeps zero dependency
// on internal/queue (which pulls in modernc.org/sqlite), matching
// Ingester's own precedent of a narrow interface over the real subsystem
// rather than importing it wholesale. See QueueReader.
type QueueCounts struct {
	AwaitingUpload int
	AwaitingRebase int
	Failed         int
	Done           int
	PendingBytes   int64
}

// Pending is the total row count still needing some action -- mirrors
// queue.Counts.Pending()'s own method-not-field shape so the two can never
// independently drift.
func (c QueueCounts) Pending() int {
	return c.AwaitingUpload + c.AwaitingRebase
}

// QueueReader is the subset of *queue.Store's surface the tray needs for a
// live status readout: an interface, like Ingester/SelfUpdater/Settings,
// so internal/tray stays unit-testable with a fake and never needs to
// import internal/queue itself. Counts is cheap by design (queue.Store's
// own doc comment) -- safe to call on every Status() request and tray
// menu-refresh tick.
type QueueReader interface {
	Counts(ctx context.Context) (QueueCounts, error)
}

// DrainSummary is a condensed, human-readable view of one
// internal/ingest.Drain pass -- the tray-local mirror of
// internal/ingest.DrainStats, for the same reason QueueCounts mirrors
// internal/queue.Counts. At is set by Runner.TriggerDrain, not by the
// Drainer implementation, so it always reflects when the tray actually
// ran the pass.
type DrainSummary struct {
	At                time.Time
	HandshakeOK       bool
	NodeCreatedSent   int
	ArchiveCopiesDone int
	RebasesDone       int
	RebasesFailed     int
	Remaining         int
	Err               error
}

// Drainer is the subset of internal/ingest.Drain's behavior the tray runs
// on its own timer and from the "Drain queue now" menu item (see
// Runner.TriggerDrain).
type Drainer interface {
	Drain(ctx context.Context) (DrainSummary, error)
}

// PruneSummary is a condensed view of one internal/prune.Pass pass -- the
// tray-local mirror of internal/prune.Stats. At is set by
// Runner.TriggerPrune, same reasoning as DrainSummary.At.
type PruneSummary struct {
	At         time.Time
	Evaluated  int
	Pruned     int
	FreedBytes int64
	Err        error
}

// Pruner is the subset of internal/prune.Pass's behavior the tray runs on
// its own timer and from the "Prune now" menu item (see
// Runner.TriggerPrune). Wired only when prune.enabled is true in config --
// see QueueStatus.PruneEnabled.
type Pruner interface {
	Prune(ctx context.Context) (PruneSummary, error)
}

// QueueStatus is what the status page and tray menu render for the
// offline queue -- replacing the M2-era QueueStatusStub now that
// internal/queue is wired in (issue #32).
//
// Configured=false is the honest "not configured" signal (no
// offline.queueDbPath set, so the tray has no QueueReader at all) --
// deliberately distinct from Err, which means queue.db exists and opened
// but a Counts() call itself failed (e.g. permissions, corruption after
// the fact). Neither case fabricates a 0, preserving the invariant
// QueueStatusStub used to hold as a literal string: a badge must never
// read as healthy when the truth is "unknown."
type QueueStatus struct {
	Configured   bool
	Counts       QueueCounts
	Err          error
	PruneEnabled bool
	LastDrain    *DrainSummary
	LastPrune    *PruneSummary
}
