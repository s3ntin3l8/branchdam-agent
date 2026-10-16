package tray

import (
	"context"
	"encoding/json"
	"errors"
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
	At          time.Time
	HandshakeOK bool
	// LastHandshakeAt is the timestamp the agent last completed a drain
	// pass with a successful server handshake -- the freshness signal the
	// status page's "last handshake" line renders. A zero value means
	// "never completed a successful handshake" and the template suppresses
	// the line in that case (issue #109 / audit F-13 follow-up; the
	// boolean HandshakeOK above collapses "drained 5 minutes ago" and
	// "drained 3 weeks ago" into the same true value, which is what this
	// field disambiguates). The Drainer implementation stamps this only
	// when HandshakeOK is true, and Runner.TriggerDrain additionally
	// preserves the prior successful stamp when a newer pass's handshake
	// failed -- a fresh failed pass must not erase "successful 4h ago".
	LastHandshakeAt     time.Time `json:"lastHandshakeAt,omitempty"`
	NodeCreatedSent     int
	ArchiveCopiesDone   int
	ArchiveCopiesFailed int
	RebasesDone         int
	RebasesFailed       int
	Remaining           int
	Err                 error
}

// MarshalJSON renders Err as a string -- see
// IngestSummary.MarshalJSON's doc comment (tray.go) for why and how.
func (s DrainSummary) MarshalJSON() ([]byte, error) {
	type alias DrainSummary
	return json.Marshal(struct {
		alias
		Err string `json:"Err,omitempty"`
	}{alias: alias(s), Err: errString(s.Err)})
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
	At time.Time
	// Duration is how long the pass took (stamped by TriggerPrune), what
	// telemetry reports as lastPruneDurationMs. JSON carries it as
	// durationMs (see MarshalJSON), never as raw nanoseconds.
	Duration   time.Duration `json:"-"`
	Evaluated  int
	Pruned     int
	FreedBytes int64
	Err        error
}

// MarshalJSON renders Err as a string -- see
// IngestSummary.MarshalJSON's doc comment (tray.go) for why and how.
func (s PruneSummary) MarshalJSON() ([]byte, error) {
	type alias PruneSummary
	return json.Marshal(struct {
		alias
		DurationMs int64  `json:"durationMs,omitempty"`
		Err        string `json:"Err,omitempty"`
	}{alias: alias(s), DurationMs: s.Duration.Milliseconds(), Err: errString(s.Err)})
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

// MarshalJSON renders Err as a string -- see
// IngestSummary.MarshalJSON's doc comment (tray.go) for why and how.
// LastDrain/LastPrune need no special handling here: DrainSummary and
// PruneSummary each have their own MarshalJSON, which encoding/json
// invokes automatically through the pointer.
func (s QueueStatus) MarshalJSON() ([]byte, error) {
	type alias QueueStatus
	return json.Marshal(struct {
		alias
		Err string `json:"Err,omitempty"`
	}{alias: alias(s), Err: errString(s.Err)})
}

// FailedQueueRow is one queue.db row with a terminally FAILED archive copy
// or rebase -- the tray-local mirror of the fields of internal/queue.Record
// the window's failed-rows list shows, duplicated for the same reason
// QueueCounts is. Step names which step failed ("archive_copy" or
// "rebase"; an archive-copy failure ends the row before rebase ever runs)
// and LastError is that step's recorded reason.
type FailedQueueRow struct {
	NodeUUID    string `json:"nodeUuid"`
	FileName    string `json:"fileName"`
	SourcePath  string `json:"sourcePath"`
	LocalPath   string `json:"localPath"`
	ArchivePath string `json:"archivePath"`
	SizeBytes   int64  `json:"sizeBytes"`
	Step        string `json:"step"`
	LastError   string `json:"lastError"`
	Attempts    int    `json:"attempts"`
	CreatedAt   int64  `json:"createdAtUnix"`
}

// Failed-step names reported in FailedQueueRow.Step.
const (
	FailedStepArchiveCopy = "archive_copy"
	FailedStepRebase      = "rebase"
)

// QueueRetryCounts is the tray-local mirror of internal/queue.ResetCounts:
// how many FAILED steps a retry put back to PENDING.
type QueueRetryCounts struct {
	ArchiveCopies int64 `json:"archiveCopies"`
	Rebases       int64 `json:"rebases"`
}

// QueueFailureManager is the optional half of a QueueReader that can list
// FAILED rows and re-queue them (internal/queue.Store's FailedRows,
// ResetFailedNode and ResetFailed, the same reset `queue-drain
// -retry-failed` runs). A separate interface rather than more QueueReader
// methods so every existing Counts-only fake keeps compiling; Runner
// type-asserts for it and reports ErrQueueRetryUnsupported when absent.
type QueueFailureManager interface {
	FailedRows(ctx context.Context, limit int) ([]FailedQueueRow, error)
	// RetryFailed resets nodeUUID's FAILED steps, or every row's when
	// nodeUUID is "".
	RetryFailed(ctx context.Context, nodeUUID string) (QueueRetryCounts, error)
}

// ErrQueueNotConfigured is returned by the failed-row methods when no
// offline queue is wired (offline.queueDbPath unset).
var ErrQueueNotConfigured = errors.New("tray: offline queue is not configured")

// ErrQueueRetryUnsupported is returned when the wired QueueReader cannot
// list or reset failed rows.
var ErrQueueRetryUnsupported = errors.New("tray: offline queue does not support listing or retrying failed rows")

// failedQueueReadTimeout bounds a failed-rows read the same way
// statusQueueReadTimeout bounds Status()'s Counts call.
const failedQueueReadTimeout = statusQueueReadTimeout

// queueFailureManager returns the wired QueueReader's QueueFailureManager
// half, or the error explaining why there is none.
func (r *Runner) queueFailureManager() (QueueFailureManager, error) {
	r.mu.Lock()
	reader := r.queueReader
	r.mu.Unlock()
	if reader == nil {
		return nil, ErrQueueNotConfigured
	}
	m, ok := reader.(QueueFailureManager)
	if !ok {
		return nil, ErrQueueRetryUnsupported
	}
	return m, nil
}

// FailedQueueRows lists up to limit FAILED queue rows (limit <= 0: all),
// oldest first, for the window's failed-rows list.
func (r *Runner) FailedQueueRows(ctx context.Context, limit int) ([]FailedQueueRow, error) {
	m, err := r.queueFailureManager()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, failedQueueReadTimeout)
	defer cancel()
	return m.FailedRows(ctx, limit)
}

// RetryFailedQueueRows puts nodeUUID's FAILED steps (every row's when
// nodeUUID is "") back to PENDING so the next drain pass retries them. It
// needs no lock of its own: the reset only touches rows already FAILED,
// which a drain pass never selects (they are terminal), and queue.db is a
// single-connection store, so a concurrent drain can neither lose the
// reset nor see half of it. Deliberately not gated on Paused(): re-queuing
// does no network I/O, and the paused drain simply picks the rows up once
// resumed.
func (r *Runner) RetryFailedQueueRows(ctx context.Context, nodeUUID string) (QueueRetryCounts, error) {
	m, err := r.queueFailureManager()
	if err != nil {
		return QueueRetryCounts{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, failedQueueReadTimeout)
	defer cancel()
	return m.RetryFailed(ctx, nodeUUID)
}
