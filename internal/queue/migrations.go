package queue

import (
	"fmt"
)

// migrations is the forward-only list of schema upgrades. Each entry says
// "if user_version == fromVersion, run sql and set user_version to the next
// version." Migrations run in order; a v0 queue.db migrates through every
// entry. A v1 queue.db (user_version >= 1) runs no migrations at all.
//
// Forward-only: there is no rollback path. If a migration fails partway,
// the pragma is not bumped and the next Open() retries it (the DDL uses
// IF NOT EXISTS / CREATE INDEX IF NOT EXISTS, so partial application is
// safe to retry).
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
}

// migrate reads PRAGMA user_version and applies every pending migration to
// bring the database up to currentSchemaVersion. The DDL in each migration
// uses IF NOT EXISTS / CREATE INDEX IF NOT EXISTS, so re-running a migration
// that was partially applied (e.g. the process crashed after CREATE TABLE
// succeeded but before the index was created) is safe.
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("queue: read user_version: %w", err)
	}

	for _, m := range migrations {
		if version != m.fromVersion {
			continue
		}
		if _, err := s.db.Exec(m.sql); err != nil {
			return fmt.Errorf("queue: migrate v%d→%d: %w", m.fromVersion, m.fromVersion+1, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.fromVersion+1)); err != nil {
			return fmt.Errorf("queue: set user_version to %d: %w", m.fromVersion+1, err)
		}
		version = m.fromVersion + 1
	}

	if version < currentSchemaVersion {
		return fmt.Errorf("queue: unknown schema version %d (expected %d)", version, currentSchemaVersion)
	}
	return nil
}
