// Package queue is branchdam-agent's offline event queue: queue.db
// (modernc.org/sqlite, pure Go). Every intended event is persisted here
// BEFORE any network call -- the client-minted node UUID is the stable
// identity that survives a crash at any point, and never gets re-minted on
// restart (a re-sent EVENT_NODE_CREATED for an existing nodeUuid is a
// silent no-op server-side; internal/branchdam's PostNodeCreated doc
// comment). See docs/offline-queue.md for the full state-machine writeup.
//
// One row per ingested file (queue_nodes), carrying three independent
// per-step status columns -- node_created, archive_copy, rebase -- because
// those three steps can complete in any order relative to each other except
// one hard constraint: rebase never starts until archive_copy is DONE (the
// whole reason this package exists -- see internal/ingest/drain.go's
// MinRebaseDwell doc comment for the additional dwell gate on top of that).
package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// currentSchemaVersion is the latest schema version Open/migrate must bring
// a queue.db to. Bump this and add a migration entry to the migrations
// slice whenever the schema changes in a backward-incompatible way.
const currentSchemaVersion = 1

// Status values for the three independent per-step columns.
const (
	StatusPending = "PENDING"
	// StatusSubmitted applies to node_created only: the server accepted the
	// POST (202) and enqueued it -- NOT confirmation the drainer applied it
	// (plan contract gap 3: no failure feedback channel). See
	// internal/ingest/drain.go's MinRebaseDwell for why rebase still waits
	// after this.
	StatusSubmitted = "SUBMITTED"
	// StatusDone applies to archive_copy and rebase: the step is verifiably
	// complete (a cache-defeating re-read for archive_copy; a 200 response
	// for rebase).
	StatusDone = "DONE"
	// StatusSkipped applies to node_created and rebase on a SIDECAR-kind
	// row: sidecars are never server-tracked nodes (M1a's ingestFile
	// behavior, preserved here), so those two steps never run for them --
	// only archive_copy does.
	StatusSkipped = "SKIPPED"
	// StatusFailed applies to rebase only: a classified-fatal server error
	// (e.g. ArchivedNode) that retrying cannot fix. Terminal -- Drain never
	// reattempts a FAILED row. node_created/archive_copy have no FAILED
	// state; they retry indefinitely with backoff, since every failure mode
	// on those two steps (network down, NAS unmounted) is expected to
	// eventually clear.
	StatusFailed = "FAILED"
)

// Kind distinguishes a full media pipeline row (node_created + archive_copy
// + rebase, submitted to branchDAM as a tracked node) from a sidecar row
// (.xmp/.srt: archive_copy only, matching M1a's ingestFile -- a sidecar is
// copied to both destinations but never gets its own EVENT_NODE_CREATED).
const (
	KindMedia   = "MEDIA"
	KindSidecar = "SIDECAR"
)

// Record is one queue_nodes row.
type Record struct {
	ID                         int64
	NodeUUID                   string
	Kind                       string
	SourcePath                 string
	LocalPath                  string
	ArchivePath                string // workstation path -- final Tier-3 destination
	ArchiveContainerPath       string // container-path form of ArchivePath, for POST /rebase's targetPath
	Tier0ContainerPath         string // container path used for EVENT_NODE_CREATED; "" for SIDECAR
	FileName                   string
	FileExt                    string
	SizeBytes                  int64
	MtimeUnix                  int64
	FullHash                   string
	FastHash                   string
	NodeCreatedPayloadJSON     string // "" for SIDECAR
	NodeCreatedStatus          string
	NodeCreatedEventID         string
	NodeCreatedSubmittedAtUnix int64
	NodeCreatedAttempts        int
	NodeCreatedNextAttemptUnix int64
	NodeCreatedLastError       string
	ArchiveCopyStatus          string
	ArchiveCopyAttempts        int
	ArchiveCopyNextAttemptUnix int64
	ArchiveCopyLastError       string
	RebaseStatus               string
	RebaseAttempts             int
	RebaseNextAttemptUnix      int64
	RebaseLastError            string
	CreatedAtUnix              int64
}

// Done reports whether every step this record needs has reached a terminal
// state (DONE/SKIPPED for archive_copy+rebase; FAILED counts as terminal
// too, even though it is not success, so Pending() stops returning it).
func (r Record) Done() bool {
	archiveDone := r.ArchiveCopyStatus == StatusDone
	rebaseDone := r.RebaseStatus == StatusDone || r.RebaseStatus == StatusSkipped || r.RebaseStatus == StatusFailed
	return archiveDone && rebaseDone
}

// Store wraps queue.db. A single *sql.DB with SetMaxOpenConns(1): SQLite
// pragmas (journal_mode, synchronous, busy_timeout) are connection-scoped,
// not database-scoped, so a pool of more than one connection would apply
// them to whichever connection happened to run the PRAGMA statement and
// leave the others at SQLite's defaults -- the same trap class as
// branchDAM's own PRAGMA foreign_keys invariant (see that repo's AGENTS.md).
// One connection also matches this package's actual concurrency
// requirement: one agent process, one queue.db, never written from two
// goroutines at once.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the queue.db at path, sets its durability
// pragmas, and ensures the schema exists.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	// journal_mode=WAL + synchronous=FULL: every status-transition UPDATE in
	// this file (and the initial INSERT in InsertPending) fsyncs before its
	// call returns -- the durability floor the crash-safety AC depends on.
	// WAL alone (synchronous=NORMAL, SQLite's own WAL default) can lose the
	// last commit on a power-loss-class crash; FULL is the one that doesn't.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := s.db.Exec(p); err != nil {
			return fmt.Errorf("queue: set pragma %q: %w", p, err)
		}
	}

	if err := s.migrate(); err != nil {
		return err
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// NewRecord is the input to InsertPending: everything known about a file at
// the moment it is queued, before any network call.
type NewRecord struct {
	NodeUUID               string
	Kind                   string
	SourcePath             string
	LocalPath              string
	ArchivePath            string
	ArchiveContainerPath   string
	Tier0ContainerPath     string
	FileName               string
	FileExt                string
	SizeBytes              int64
	MtimeUnix              int64
	FullHash               string
	FastHash               string
	NodeCreatedPayloadJSON string
}

// InsertPending inserts rec as a fresh row with every step status at its
// initial value (PENDING for MEDIA's three steps; PENDING for SIDECAR's
// archive_copy, SKIPPED for the other two, since a sidecar never gets a
// tracked node). This is the durability boundary the whole package exists
// for: the caller must not attempt any network call for this file before
// this insert has returned successfully -- see internal/ingest/offline.go's
// ingestFileOffline for the call site and why the ordering matters (a crash
// between a successful POST and this insert would leave a server-side node
// nothing local remembers, with no read endpoint to recover it).
func (s *Store) InsertPending(ctx context.Context, rec NewRecord) error {
	nodeCreatedStatus := StatusPending
	rebaseStatus := StatusPending
	if rec.Kind == KindSidecar {
		nodeCreatedStatus = StatusSkipped
		rebaseStatus = StatusSkipped
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO queue_nodes (
	node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
	tier0_container_path, file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash,
	node_created_payload_json, node_created_status, archive_copy_status, rebase_status, created_at_unix
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.NodeUUID, rec.Kind, rec.SourcePath, rec.LocalPath, rec.ArchivePath, rec.ArchiveContainerPath,
		rec.Tier0ContainerPath, rec.FileName, rec.FileExt, rec.SizeBytes, rec.MtimeUnix, rec.FullHash, rec.FastHash,
		rec.NodeCreatedPayloadJSON, nodeCreatedStatus, StatusPending, rebaseStatus, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("queue: insert pending row for %s: %w", rec.SourcePath, err)
	}
	return nil
}

// ByNodeUUID returns the row for nodeUUID, or ok=false if none exists.
func (s *Store) ByNodeUUID(ctx context.Context, nodeUUID string) (Record, bool, error) {
	row := s.db.QueryRowContext(ctx, selectCols+" WHERE node_uuid = ?", nodeUUID)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows { //nolint:errorlint // database/sql sentinel, never wrapped
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("queue: lookup by node_uuid %s: %w", nodeUUID, err)
	}
	return rec, true, nil
}

// BySourcePath returns the most recently queued row for sourcePath, or
// ok=false if none exists. Used to detect a re-run over a card that was
// already (partially) queued -- see internal/ingest/offline.go's
// ingestFileOffline doc comment for the resume-vs-refuse decision this
// backs.
func (s *Store) BySourcePath(ctx context.Context, sourcePath string) (Record, bool, error) {
	row := s.db.QueryRowContext(ctx, selectCols+" WHERE source_path = ? ORDER BY id DESC LIMIT 1", sourcePath)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows { //nolint:errorlint // database/sql sentinel, never wrapped
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("queue: lookup by source_path %s: %w", sourcePath, err)
	}
	return rec, true, nil
}

// ByLocalPath returns the row with local_path = localPath, or ok=false if none exists.
func (s *Store) ByLocalPath(ctx context.Context, localPath string) (Record, bool, error) {
	row := s.db.QueryRowContext(ctx, selectCols+" WHERE local_path = ? ORDER BY id DESC LIMIT 1", localPath)
	rec, err := scanRecord(row)
	if err == sql.ErrNoRows { //nolint:errorlint // database/sql sentinel, never wrapped
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("queue: lookup by local_path %s: %w", localPath, err)
	}
	return rec, true, nil
}

// terminalSQL is the row-level predicate matching Record.Done(): every
// step this record needs has reached a terminal state, including FAILED
// (permanently broken, but no longer something Drain -- or Pending's
// caller -- should keep retrying). Shared by Pending's WHERE NOT(...) and
// Counts' aggregate below, rather than each re-deriving it, so the two
// can't drift out of sync the way Record.Done() and Pending's own
// previously-inline SQL already had (a real duplication found while
// building Counts).
const terminalSQL = `archive_copy_status = 'DONE' AND rebase_status IN ('DONE', 'SKIPPED', 'FAILED')`

// Pending returns every row not yet Done(), oldest first -- the set Drain
// works through on each pass.
func (s *Store) Pending(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+`
WHERE NOT (`+terminalSQL+`)
ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("queue: list pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		rec, err := scanRecordRows(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: scan pending row: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// All returns every row regardless of status, oldest first -- used by tests
// asserting exactly-once semantics (no duplicate rows for the same source
// file across a crash/restart).
func (s *Store) All(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+" ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("queue: list all: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		rec, err := scanRecordRows(rows)
		if err != nil {
			return nil, fmt.Errorf("queue: scan row: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Counts is a cheap, aggregate-only readout of queue_nodes -- unlike
// Pending/All, it never materializes a row's full payload JSON or error
// strings, so a tray polling this on a timer over a large backlog isn't
// paying to scan and marshal thousands of rows just to show a badge.
//
// Failed is reported separately from Done on purpose: Record.Done() (and
// terminalSQL above) treats a permanently FAILED rebase as terminal so
// Pending() stops returning it, but that is not the same as success --
// a badge driven off "Pending() is empty, so we're done" would read
// green while rows sit permanently broken. Done here means genuinely
// succeeded (archive_copy DONE, rebase DONE or SKIPPED); Failed means
// rebase_status = FAILED, whichever else is true of the row.
type Counts struct {
	// AwaitingUpload counts rows whose archive copy hasn't started
	// (archive_copy_status = PENDING) -- MEDIA and SIDECAR rows alike.
	AwaitingUpload int
	// AwaitingRebase counts MEDIA rows whose archive copy is done but
	// rebase hasn't happened yet. A SIDECAR row's rebase_status is
	// SKIPPED from insert (see InsertPending), so it never appears here.
	AwaitingRebase int
	// Failed counts rows with a permanently failed (terminal) rebase --
	// see the type doc comment for why this is never folded into Done.
	Failed int
	// Done counts rows that genuinely completed: archive copy done and
	// rebase done or skipped -- explicitly NOT failed.
	Done int
	// PendingBytes sums SizeBytes over AwaitingUpload rows -- the backlog
	// still waiting to be copied to the archive, in bytes.
	PendingBytes int64
}

// Pending is the total row count still needing some action --
// AwaitingUpload plus AwaitingRebase. Named as a method, not a field, so
// it can never independently drift from the two counts it's built from.
func (c Counts) Pending() int {
	return c.AwaitingUpload + c.AwaitingRebase
}

// Counts runs one aggregate query over queue_nodes -- see the Counts type
// doc comment for why this exists instead of len(Pending()) or bucketing
// All() in Go.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT
	COALESCE(SUM(CASE WHEN archive_copy_status = 'PENDING' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN archive_copy_status = 'DONE' AND rebase_status = 'PENDING' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN rebase_status = 'FAILED' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN (`+terminalSQL+`) AND rebase_status != 'FAILED' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN archive_copy_status = 'PENDING' THEN size_bytes ELSE 0 END), 0)
FROM queue_nodes`)

	var c Counts
	if err := row.Scan(&c.AwaitingUpload, &c.AwaitingRebase, &c.Failed, &c.Done, &c.PendingBytes); err != nil {
		return Counts{}, fmt.Errorf("queue: counts: %w", err)
	}
	return c, nil
}

// MarkNodeCreatedSubmitted records that EVENT_NODE_CREATED was accepted
// (202) by the server -- NOT that the drainer applied it. submittedAt backs
// internal/ingest/drain.go's MinRebaseDwell gate.
func (s *Store) MarkNodeCreatedSubmitted(ctx context.Context, nodeUUID, eventID string, submittedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET node_created_status = ?, node_created_event_id = ?, node_created_submitted_at_unix = ? WHERE node_uuid = ?`,
		StatusSubmitted, eventID, submittedAt.Unix(), nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark node_created submitted for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkNodeCreatedAttempt records a failed EVENT_NODE_CREATED attempt:
// increments the attempt counter, stores errMsg, and schedules the next
// attempt via nextAttempt (exponential backoff -- see drain.go's
// backoffFor). The row stays PENDING; node_created has no terminal FAILED
// state (see StatusFailed's doc comment).
func (s *Store) MarkNodeCreatedAttempt(ctx context.Context, nodeUUID, errMsg string, nextAttempt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET node_created_attempts = node_created_attempts + 1, node_created_last_error = ?, node_created_next_attempt_unix = ? WHERE node_uuid = ?`,
		errMsg, nextAttempt.Unix(), nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: record node_created attempt for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkArchiveCopyDone records that the archive copy landed and verified.
func (s *Store) MarkArchiveCopyDone(ctx context.Context, nodeUUID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE queue_nodes SET archive_copy_status = ? WHERE node_uuid = ?`, StatusDone, nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark archive_copy done for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkArchiveCopyAttempt records a failed archive-copy attempt (e.g. the
// archive root still isn't reachable). The row stays PENDING.
func (s *Store) MarkArchiveCopyAttempt(ctx context.Context, nodeUUID, errMsg string, nextAttempt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET archive_copy_attempts = archive_copy_attempts + 1, archive_copy_last_error = ?, archive_copy_next_attempt_unix = ? WHERE node_uuid = ?`,
		errMsg, nextAttempt.Unix(), nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: record archive_copy attempt for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkRebaseDone records a successful POST /api/v1/agent/rebase.
func (s *Store) MarkRebaseDone(ctx context.Context, nodeUUID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE queue_nodes SET rebase_status = ? WHERE node_uuid = ?`, StatusDone, nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark rebase done for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkRebaseAttempt records a transient rebase failure (retry later).
func (s *Store) MarkRebaseAttempt(ctx context.Context, nodeUUID, errMsg string, nextAttempt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET rebase_attempts = rebase_attempts + 1, rebase_last_error = ?, rebase_next_attempt_unix = ? WHERE node_uuid = ?`,
		errMsg, nextAttempt.Unix(), nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: record rebase attempt for %s: %w", nodeUUID, err)
	}
	return nil
}

// MarkRebaseFailed records a terminal (classified-fatal) rebase failure --
// e.g. the node is ARCHIVED server-side. Drain never reattempts a FAILED
// row; an operator has to intervene.
func (s *Store) MarkRebaseFailed(ctx context.Context, nodeUUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET rebase_status = ?, rebase_last_error = ? WHERE node_uuid = ?`,
		StatusFailed, errMsg, nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark rebase failed for %s: %w", nodeUUID, err)
	}
	return nil
}

const selectCols = `SELECT
	id, node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
	tier0_container_path, file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash,
	node_created_payload_json, node_created_status, node_created_event_id,
	node_created_submitted_at_unix, node_created_attempts, node_created_next_attempt_unix, node_created_last_error,
	archive_copy_status, archive_copy_attempts, archive_copy_next_attempt_unix, archive_copy_last_error,
	rebase_status, rebase_attempts, rebase_next_attempt_unix, rebase_last_error,
	created_at_unix
FROM queue_nodes`

// rowScanner is the subset of *sql.Row / *sql.Rows this package needs, so
// scanRecord/scanRecordRows can share one field list against either.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row *sql.Row) (Record, error) {
	return scan(row)
}

func scanRecordRows(rows *sql.Rows) (Record, error) {
	return scan(rows)
}

func scan(s rowScanner) (Record, error) {
	var r Record
	err := s.Scan(
		&r.ID, &r.NodeUUID, &r.Kind, &r.SourcePath, &r.LocalPath, &r.ArchivePath, &r.ArchiveContainerPath,
		&r.Tier0ContainerPath, &r.FileName, &r.FileExt, &r.SizeBytes, &r.MtimeUnix, &r.FullHash, &r.FastHash,
		&r.NodeCreatedPayloadJSON, &r.NodeCreatedStatus, &r.NodeCreatedEventID,
		&r.NodeCreatedSubmittedAtUnix, &r.NodeCreatedAttempts, &r.NodeCreatedNextAttemptUnix, &r.NodeCreatedLastError,
		&r.ArchiveCopyStatus, &r.ArchiveCopyAttempts, &r.ArchiveCopyNextAttemptUnix, &r.ArchiveCopyLastError,
		&r.RebaseStatus, &r.RebaseAttempts, &r.RebaseNextAttemptUnix, &r.RebaseLastError,
		&r.CreatedAtUnix,
	)
	return r, err
}
