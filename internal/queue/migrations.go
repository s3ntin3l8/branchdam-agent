package queue

import (
	"fmt"
)

// migrations is the forward-only list of schema upgrades. Each entry says
// "if user_version == fromVersion, run sql and set user_version to the next
// version." Migrations run in order; a v0 queue.db migrates through every
// entry. A v1 queue.db (user_version >= 1) runs no migrations at all.
//
// Forward-only: there is no rollback path. Each entry runs together with its
// user_version bump in one transaction (SQLite DDL and the user_version
// pragma are both transactional), so a migration that fails partway leaves
// the database at the old version, untouched, and the next Open() retries it
// from the start -- which an ALTER TABLE ... ADD COLUMN could not survive if
// the column had been added and the pragma not yet bumped.
var migrations = []struct {
	fromVersion int
	sql         string
}{
	{
		fromVersion: 0,
		// Status-column index: Pending() filters on
		// archive_copy_status/rebase_status via WHERE NOT(...), and Counts()
		// aggregates on the same two columns. Leading with
		// archive_copy_status maximizes leftmost-prefix utilization for the
		// most selective filter (PENDING vs non-PENDING).
		//
		// Deliberately NOT keyed on node_created_status (issue #161):
		// every query in store.go filters/aggregates on
		// archive_copy_status and/or rebase_status only --
		// node_created_status is set/updated by MarkNodeCreatedDone
		// (store.go:380) but is never used in a WHERE/GROUP BY
		// clause, so adding it as the index's leading column would
		// widen the index without improving any query plan. The
		// narrower (archive_copy_status, rebase_status) index is
		// functionally correct and faster.
		sql: `
CREATE TABLE IF NOT EXISTS queue_nodes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	node_uuid TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL CHECK (kind IN ('MEDIA','SIDECAR')),
	source_path TEXT NOT NULL,
	local_path TEXT NOT NULL,
	archive_path TEXT NOT NULL,
	archive_container_path TEXT NOT NULL,
	tier0_container_path TEXT NOT NULL DEFAULT '',
	file_name TEXT NOT NULL,
	file_ext TEXT NOT NULL,
	size_bytes INTEGER NOT NULL,
	mtime_unix INTEGER NOT NULL,
	full_hash TEXT NOT NULL,
	fast_hash TEXT NOT NULL,
	node_created_payload_json TEXT NOT NULL DEFAULT '',
	node_created_status TEXT NOT NULL DEFAULT 'PENDING',
	node_created_event_id TEXT NOT NULL DEFAULT '',
	node_created_submitted_at_unix INTEGER NOT NULL DEFAULT 0,
	node_created_attempts INTEGER NOT NULL DEFAULT 0,
	node_created_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	node_created_last_error TEXT NOT NULL DEFAULT '',
	archive_copy_status TEXT NOT NULL DEFAULT 'PENDING',
	archive_copy_attempts INTEGER NOT NULL DEFAULT 0,
	archive_copy_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	archive_copy_last_error TEXT NOT NULL DEFAULT '',
	rebase_status TEXT NOT NULL DEFAULT 'PENDING',
	rebase_attempts INTEGER NOT NULL DEFAULT 0,
	rebase_next_attempt_unix INTEGER NOT NULL DEFAULT 0,
	rebase_last_error TEXT NOT NULL DEFAULT '',
	created_at_unix INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_queue_nodes_source_path ON queue_nodes(source_path);
CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_nodes_node_uuid ON queue_nodes(node_uuid);
CREATE INDEX IF NOT EXISTS idx_queue_nodes_status ON queue_nodes(archive_copy_status, rebase_status);
`,
	},
	{
		fromVersion: 1,
		sql: `
ALTER TABLE queue_nodes ADD COLUMN node_created_event_uuid TEXT NOT NULL DEFAULT '';
UPDATE queue_nodes SET node_created_event_uuid = node_uuid WHERE node_created_event_uuid = '';
`,
	},
	{
		// origin: which ingest path wrote the row (store.go's OriginOffline
		// / OriginOnline). Every row before this version came from
		// IngestCardOffline -- online ingest never wrote queue rows -- so
		// the OFFLINE default is exact for them.
		fromVersion: 2,
		sql: `
ALTER TABLE queue_nodes ADD COLUMN origin TEXT NOT NULL DEFAULT 'OFFLINE';
`,
	},
	{
		// Upload stage: a metered-deferred upload-mode file (OriginUpload)
		// is queued with the upload request's inputs and a fourth step,
		// upload_status, that Drain runs. Every earlier row is not an
		// upload row, so SKIPPED (no upload step) is exact for them. The
		// sidecar link (master_node_uuid, sidecar_tail, sidecar_by_name)
		// lets Drain name a sidecar after its master's server-assigned
		// name; server_node_uuid / server_relative_path record the
		// upload's answer. node_uuid stays the row's client-minted key --
		// an UPLOAD row's is never sent to the server.
		fromVersion: 3,
		sql: `
ALTER TABLE queue_nodes ADD COLUMN upload_status TEXT NOT NULL DEFAULT 'SKIPPED';
ALTER TABLE queue_nodes ADD COLUMN upload_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_nodes ADD COLUMN upload_next_attempt_unix INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_nodes ADD COLUMN upload_last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN upload_name TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN camera_model TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN capture_timestamp INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_nodes ADD COLUMN source_path_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN master_node_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN sidecar_tail TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN sidecar_by_name INTEGER NOT NULL DEFAULT 0;
ALTER TABLE queue_nodes ADD COLUMN local_edit_root TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN server_node_uuid TEXT NOT NULL DEFAULT '';
ALTER TABLE queue_nodes ADD COLUMN server_relative_path TEXT NOT NULL DEFAULT '';
`,
	},
}

// migrate reads PRAGMA user_version and applies every pending migration to
// bring the database up to currentSchemaVersion. Each migration and its
// user_version bump commit together, so a crash mid-migration re-runs it
// whole against the old schema.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("queue: read user_version: %w", err)
	}

	for _, m := range migrations {
		if version != m.fromVersion {
			continue
		}
		if err := s.applyMigration(m.fromVersion, m.sql); err != nil {
			return err
		}
		version = m.fromVersion + 1
	}

	if version < currentSchemaVersion {
		return fmt.Errorf("queue: unknown schema version %d (expected %d)", version, currentSchemaVersion)
	}
	return nil
}

// applyMigration runs one migration's sql and the user_version bump in a
// single transaction.
func (s *Store) applyMigration(fromVersion int, sqlText string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("queue: migrate v%d→%d: begin: %w", fromVersion, fromVersion+1, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(sqlText); err != nil {
		return fmt.Errorf("queue: migrate v%d→%d: %w", fromVersion, fromVersion+1, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", fromVersion+1)); err != nil {
		return fmt.Errorf("queue: set user_version to %d: %w", fromVersion+1, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: migrate v%d→%d: commit: %w", fromVersion, fromVersion+1, err)
	}
	return nil
}
