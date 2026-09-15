package resolve

import (
	"context"
	"database/sql"
)

// openTestDB creates a minimal SQLite database at path for testing.
// It uses database/sql directly to create the file, then returns a
// read-only *DB handle via Open. Shared between discover_test.go and
// resolve_test.go (sync tests), so it lives in its own file rather
// than either test's own.
func openTestDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS _probe (id INTEGER PRIMARY KEY)"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return Open(context.Background(), "file:"+path+"?mode=ro")
}
