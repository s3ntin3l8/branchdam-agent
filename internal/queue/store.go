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
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// currentSchemaVersion is the latest schema version Open/migrate must bring
// a queue.db to. Bump this and add a migration entry to the migrations
// slice whenever the schema changes in a backward-incompatible way.
const currentSchemaVersion = 3

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
	// StatusFailed is terminal -- Drain never reattempts a FAILED step.
	// On rebase it means a classified-fatal server error (e.g.
	// ArchivedNode) that retrying cannot fix. On archive_copy it means the
	// copy can never succeed without an operator: a different file already
	// occupies the final archive path (the agent never overwrites one), or
	// the local copy no longer matches the hash recorded at ingest (see
	// internal/ingest's ErrArchiveConflict / ErrLocalCopyMismatch). A row
	// whose archive copy FAILED never reaches rebase. node_created has no
	// FAILED state; it retries indefinitely with backoff, as do every other
	// archive_copy failure (network down, NAS unmounted), since those are
	// expected to eventually clear.
	//
	// FAILED rows (archive_copy or rebase) are reset to PENDING by
	// `queue-drain -retry-failed`, arriving in the A5 PR. Until it ships,
	// resetting one requires a manual UPDATE of queue.db after the
	// operator has fixed the cause.
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

// Origin says which ingest path wrote a row.
const (
	// OriginOffline is IngestCardOffline's row: only the local edit copy
	// exists at insert time; Drain copies it to the archive, submits
	// EVENT_NODE_CREATED against the Tier-0 path, then rebases it.
	OriginOffline = "OFFLINE"
	// OriginOnline is IngestCard's row: both copies were already written
	// and verified at insert time and EVENT_NODE_CREATED points straight
	// at the Tier-3 archive path, so archive_copy starts DONE and rebase
	// starts SKIPPED. The row exists so the registration survives a POST
	// that fails (server down, process killed, ingest cancelled) --
	// Drain's node_created phase retries it until the server accepts it.
	// Prune never touches an ONLINE row's local copy: online ingest has
	// always kept its edit copies, and pruning them is an offline-only
	// feature (see internal/config.PruneConfig).
	OriginOnline = "ONLINE"
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
	NodeCreatedEventUUID       string
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
	Origin                     string // OriginOffline or OriginOnline
}

// Done reports whether every step this record needs has reached a terminal
// state (DONE/SKIPPED for archive_copy+rebase; FAILED counts as terminal
// too, even though it is not success, so Pending() stops returning it -- a
// FAILED archive copy ends the row outright, since rebase never runs
// without one). Done is not success: check Failed() too. An ONLINE row is
// not done until its EVENT_NODE_CREATED has been accepted: that
// registration is the only step it is waiting on.
func (r Record) Done() bool {
	if r.Origin == OriginOnline && r.NodeCreatedStatus == StatusPending {
		return false
	}
	if r.ArchiveCopyStatus == StatusFailed {
		return true
	}
	archiveDone := r.ArchiveCopyStatus == StatusDone
	rebaseDone := r.RebaseStatus == StatusDone || r.RebaseStatus == StatusSkipped || r.RebaseStatus == StatusFailed
	return archiveDone && rebaseDone
}

// Failed reports whether the row ended in a terminal failure on either
// its archive copy or its rebase.
func (r Record) Failed() bool {
	return r.ArchiveCopyStatus == StatusFailed || r.RebaseStatus == StatusFailed
}

// Store wraps queue.db. A single *sql.DB with SetMaxOpenConns(1): one
// agent process, one queue.db, never written from two goroutines at once.
// SQLite pragmas (synchronous, busy_timeout, foreign_keys) are
// connection-scoped, not database-scoped -- the same trap class as
// branchDAM's own PRAGMA foreign_keys invariant (see that repo's
// AGENTS.md) -- so they are set in the DSN (_pragma, see dsn), which the
// driver applies to every connection it opens. database/sql replaces a
// connection after a driver error or when the idle one is closed, and an
// Exec'd PRAGMA would have covered only the first.
type Store struct {
	db *sql.DB

	gcMu   sync.Mutex
	lastGC time.Time
}

// pragmas are applied by the driver to every new connection. The driver
// (modernc.org/sqlite's applyQueryParams) decides the order: it runs
// busy_timeout first, so the others already wait on a locked database,
// then the rest sorted.
// journal_mode=WAL + synchronous=FULL: every status-transition UPDATE in
// this file (and the initial INSERT in InsertPending) fsyncs before its
// call returns -- the durability floor the crash-safety AC depends on.
// WAL alone (synchronous=NORMAL, SQLite's own WAL default) can lose the
// last commit on a power-loss-class crash; FULL is the one that doesn't.
var pragmas = []string{
	"busy_timeout(5000)",
	"journal_mode(WAL)",
	"synchronous(FULL)",
	"foreign_keys(1)",
}

// dsn builds the driver DSN for the queue.db at path: an absolute
// "file:" URI with the path percent-escaped, plus the _pragma list. The
// driver hands a "file:" DSN to SQLite as a URI, and SQLite reads '?' as
// the start of the parameters, '#' as a fragment and '%' as an escape --
// so an unescaped "file:"+path silently opened a different file for a
// path containing any of them. url.URL escapes exactly those (and
// spaces). A Windows drive path becomes file:///C:/..., which SQLite
// accepts.
func dsn(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	q := url.Values{}
	for _, pr := range pragmas {
		q.Add("_pragma", pr)
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	return u.String(), nil
}

// Open opens (creating if needed) the queue.db at path, with its
// durability pragmas set on every connection, and ensures the schema
// exists.
func Open(path string) (*Store, error) {
	d, err := dsn(path)
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", d)
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queue: open %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) init() error {
	// Ping opens the first connection, which is where the driver runs the
	// _pragma list -- surfacing a bad path or a pragma failure here.
	if err := s.db.Ping(); err != nil {
		return err
	}
	return s.migrate()
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
	NodeCreatedEventUUID   string
	// Origin is OriginOffline (the default when empty) or OriginOnline.
	Origin string
}

// InsertPending inserts rec as a fresh row with every step status at its
// initial value (PENDING for MEDIA's three steps; PENDING for SIDECAR's
// archive_copy, SKIPPED for the other two, since a sidecar never gets a
// tracked node). An OriginOnline row starts with archive_copy DONE and
// rebase SKIPPED -- IngestCard wrote and verified both copies before
// inserting it and registers the Tier-3 path directly -- so only
// node_created is left for Drain. This is the durability boundary the whole package exists
// for: the caller must not attempt any network call for this file before
// this insert has returned successfully -- see internal/ingest/offline.go's
// ingestFileOffline for the call site and why the ordering matters (a crash
// between a successful POST and this insert would leave a server-side node
// nothing local remembers, with no read endpoint to recover it).
func (s *Store) InsertPending(ctx context.Context, rec NewRecord) error {
	nodeCreatedStatus := StatusPending
	archiveCopyStatus := StatusPending
	rebaseStatus := StatusPending
	if rec.Kind == KindSidecar {
		nodeCreatedStatus = StatusSkipped
		rebaseStatus = StatusSkipped
	}
	origin := rec.Origin
	switch origin {
	case "":
		origin = OriginOffline
	case OriginOffline:
	case OriginOnline:
		archiveCopyStatus = StatusDone
		rebaseStatus = StatusSkipped
	default:
		return fmt.Errorf("queue: insert pending row for %s: unknown origin %q", rec.SourcePath, rec.Origin)
	}

	eventUUID := rec.NodeCreatedEventUUID
	if eventUUID == "" && rec.Kind == KindMedia {
		u, err := uuid.NewV7()
		if err != nil {
			eventUUID = uuid.New().String()
		} else {
			eventUUID = u.String()
		}
	}

	_, err := s.db.ExecContext(ctx, `
INSERT INTO queue_nodes (
	node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
	tier0_container_path, file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash,
	node_created_payload_json, node_created_event_uuid, node_created_status, archive_copy_status, rebase_status, created_at_unix,
	origin
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.NodeUUID, rec.Kind, rec.SourcePath, rec.LocalPath, rec.ArchivePath, rec.ArchiveContainerPath,
		rec.Tier0ContainerPath, rec.FileName, rec.FileExt, rec.SizeBytes, rec.MtimeUnix, rec.FullHash, rec.FastHash,
		rec.NodeCreatedPayloadJSON, eventUUID, nodeCreatedStatus, archiveCopyStatus, rebaseStatus, time.Now().Unix(),
		origin,
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
// building Counts). An ONLINE row whose EVENT_NODE_CREATED is still
// PENDING is never terminal (see Record.Done).
const terminalSQL = `((archive_copy_status = 'DONE' AND rebase_status IN ('DONE', 'SKIPPED', 'FAILED')) OR archive_copy_status = 'FAILED') AND NOT (origin = 'ONLINE' AND node_created_status = 'PENDING')`

// failedSQL is the row-level predicate matching Record.Failed().
const failedSQL = `(archive_copy_status = 'FAILED' OR rebase_status = 'FAILED')`

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
// terminalSQL above) treats a permanently FAILED archive copy or rebase as
// terminal so Pending() stops returning it, but that is not the same as
// success -- a badge driven off "Pending() is empty, so we're done" would
// read green while rows sit permanently broken. Done here means genuinely
// succeeded (archive_copy DONE, rebase DONE or SKIPPED); Failed means
// archive_copy_status or rebase_status = FAILED, whichever else is true of
// the row.
type Counts struct {
	// AwaitingUpload counts rows whose archive copy hasn't started
	// (archive_copy_status = PENDING) -- MEDIA and SIDECAR rows alike.
	AwaitingUpload int
	// AwaitingRebase counts MEDIA rows whose archive copy is done but
	// whose server-side confirmation hasn't happened yet: the rebase of an
	// offline row, or the EVENT_NODE_CREATED registration of an ONLINE row
	// whose POST failed at ingest time. A SIDECAR row's rebase_status is
	// SKIPPED from insert (see InsertPending), so it never appears here.
	AwaitingRebase int
	// Failed counts rows with a permanently failed (terminal) archive copy
	// or rebase -- see the type doc comment for why this is never folded
	// into Done.
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
	COALESCE(SUM(CASE WHEN archive_copy_status = 'DONE' AND NOT (`+terminalSQL+`) THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN `+failedSQL+` THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN (`+terminalSQL+`) AND NOT `+failedSQL+` THEN 1 ELSE 0 END), 0),
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

// MarkArchiveCopyFailed records a terminal archive-copy failure -- one
// that retrying cannot fix (see StatusFailed). Drain never reattempts it;
// errMsg (archive_copy_last_error) says what an operator has to resolve.
// Once resolved, `queue-drain -retry-failed` (A5 PR) resets the row to
// PENDING; until that ships, the reset is a manual queue.db UPDATE.
func (s *Store) MarkArchiveCopyFailed(ctx context.Context, nodeUUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET archive_copy_status = ?, archive_copy_last_error = ? WHERE node_uuid = ?`,
		StatusFailed, errMsg, nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark archive_copy failed for %s: %w", nodeUUID, err)
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
// row; an operator has to intervene, then ResetFailed re-queues it.
func (s *Store) MarkRebaseFailed(ctx context.Context, nodeUUID, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE queue_nodes SET rebase_status = ?, rebase_last_error = ? WHERE node_uuid = ?`,
		StatusFailed, errMsg, nodeUUID)
	if err != nil {
		return fmt.Errorf("queue: mark rebase failed for %s: %w", nodeUUID, err)
	}
	return nil
}

// ResetCounts reports how many FAILED steps ResetFailed put back to
// PENDING.
type ResetCounts struct {
	ArchiveCopies int64
	Rebases       int64
}

// ResetFailed puts every FAILED archive_copy and every FAILED rebase back
// to PENDING with a fresh attempt counter and no backoff, so the next Drain
// pass retries them, in one transaction. FAILED is terminal to Drain, but
// a "permanent" failure is only permanent until an operator fixes its
// cause: moving aside the file that occupied the archive path or
// restoring the local copy (archive_copy), re-pairing a device, correcting
// a path mapping or restoring a node (rebase) -- and before error
// classification was status-aware, a rejected key or a 503 also failed
// rebases. The *_last_error columns are kept as a record of why the step
// failed until the retry overwrites them. A row whose archive copy was
// FAILED never reached rebase, so resetting it alone re-runs the row from
// the copy onward.
func (s *Store) ResetFailed(ctx context.Context) (ResetCounts, error) {
	var c ResetCounts
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, fmt.Errorf("queue: reset failed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE queue_nodes SET archive_copy_status = ?, archive_copy_attempts = 0, archive_copy_next_attempt_unix = 0
WHERE archive_copy_status = ?`,
		StatusPending, StatusFailed)
	if err != nil {
		return c, fmt.Errorf("queue: reset failed archive copies: %w", err)
	}
	if c.ArchiveCopies, err = res.RowsAffected(); err != nil {
		return c, fmt.Errorf("queue: reset failed archive copies: rows affected: %w", err)
	}

	res, err = tx.ExecContext(ctx,
		`UPDATE queue_nodes SET rebase_status = ?, rebase_attempts = 0, rebase_next_attempt_unix = 0
WHERE rebase_status = ?`,
		StatusPending, StatusFailed)
	if err != nil {
		return c, fmt.Errorf("queue: reset failed rebases: %w", err)
	}
	if c.Rebases, err = res.RowsAffected(); err != nil {
		return c, fmt.Errorf("queue: reset failed rebases: rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ResetCounts{}, fmt.Errorf("queue: reset failed: commit: %w", err)
	}
	return c, nil
}

// GCRetention is how old (by created_at_unix) a finished row must be
// before CollectGarbage may delete it. Deliberately long: a row is the
// agent's only memory that a source file was already ingested (offline
// resume, BySourcePath) and of who owns a local edit-copy name
// (ByLocalPath), so it is kept for a season, not a week.
const GCRetention = 90 * 24 * time.Hour

// GCInterval throttles CollectGarbageIfDue to one sweep per Store per
// interval: a sweep stats every old finished row's local copy, which is
// cheap once a day and wasteful on every 5s drain tick.
const GCInterval = 24 * time.Hour

// CollectGarbageIfDue runs CollectGarbage(ctx, now, GCRetention) if no
// sweep has completed on this Store within GCInterval of now; otherwise it
// returns (0, nil) without touching the database. A sweep that fails (a
// locked database, ctx cancelled at shutdown) does not count, so the next
// call tries again.
func (s *Store) CollectGarbageIfDue(ctx context.Context, now time.Time) (int, error) {
	s.gcMu.Lock()
	prev := s.lastGC
	if !prev.IsZero() && now.Sub(prev) < GCInterval {
		s.gcMu.Unlock()
		return 0, nil
	}
	s.lastGC = now // claimed, so a concurrent caller doesn't sweep too
	s.gcMu.Unlock()
	n, err := s.CollectGarbage(ctx, now, GCRetention)
	if err != nil {
		s.gcMu.Lock()
		if s.lastGC.Equal(now) {
			s.lastGC = prev
		}
		s.gcMu.Unlock()
	}
	return n, err
}

// CollectGarbage deletes rows nothing will need again and returns how many.
// A row is deleted only when all of these hold:
//
//   - it finished successfully (terminal and not FAILED): pending rows are
//     Drain's work list, and FAILED rows wait for an operator (or
//     `queue-drain -retry-failed`) -- neither is ever collected;
//   - it was created more than retention before now;
//   - its local edit copy is gone (Lstat says it does not exist). While the
//     copy exists the row is what prune checks before deleting it, and what
//     marks the name as the agent's own; a stat error other than "not
//     there" keeps the row.
//
// Candidates are listed first and the connection released before any stat
// (the pool has one connection), and each DELETE re-checks the
// finished-and-old predicate, so a row changed in between is kept.
func (s *Store) CollectGarbage(ctx context.Context, now time.Time, retention time.Duration) (int, error) {
	cutoff := now.Add(-retention).Unix()
	finishedOld := `(` + terminalSQL + `) AND NOT ` + failedSQL + ` AND created_at_unix < ?`

	rows, err := s.db.QueryContext(ctx, `SELECT id, local_path FROM queue_nodes WHERE `+finishedOld+` ORDER BY id ASC`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("queue: gc: list candidates: %w", err)
	}
	type candidate struct {
		id        int64
		localPath string
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.localPath); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("queue: gc: scan candidate: %w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("queue: gc: list candidates: %w", err)
	}
	_ = rows.Close()

	deleted := 0
	for _, c := range cands {
		if _, err := os.Lstat(c.localPath); !errors.Is(err, fs.ErrNotExist) {
			continue // still there (prune may want it), or unknown: keep
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM queue_nodes WHERE id = ? AND `+finishedOld, c.id, cutoff)
		if err != nil {
			return deleted, fmt.Errorf("queue: gc: delete row %d: %w", c.id, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += int(n)
		}
	}
	return deleted, nil
}

const selectCols = `SELECT
	id, node_uuid, kind, source_path, local_path, archive_path, archive_container_path,
	tier0_container_path, file_name, file_ext, size_bytes, mtime_unix, full_hash, fast_hash,
	node_created_payload_json, node_created_event_uuid, node_created_status, node_created_event_id,
	node_created_submitted_at_unix, node_created_attempts, node_created_next_attempt_unix, node_created_last_error,
	archive_copy_status, archive_copy_attempts, archive_copy_next_attempt_unix, archive_copy_last_error,
	rebase_status, rebase_attempts, rebase_next_attempt_unix, rebase_last_error,
	created_at_unix, origin
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
		&r.NodeCreatedPayloadJSON, &r.NodeCreatedEventUUID, &r.NodeCreatedStatus, &r.NodeCreatedEventID,
		&r.NodeCreatedSubmittedAtUnix, &r.NodeCreatedAttempts, &r.NodeCreatedNextAttemptUnix, &r.NodeCreatedLastError,
		&r.ArchiveCopyStatus, &r.ArchiveCopyAttempts, &r.ArchiveCopyNextAttemptUnix, &r.ArchiveCopyLastError,
		&r.RebaseStatus, &r.RebaseAttempts, &r.RebaseNextAttemptUnix, &r.RebaseLastError,
		&r.CreatedAtUnix, &r.Origin,
	)
	return r, err
}
