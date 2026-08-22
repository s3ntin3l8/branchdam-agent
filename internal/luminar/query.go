package luminar

import (
	"fmt"
	"os"
	"strings"
)

// DefaultEditSourceQuery is this package's best-effort guess at the SQL
// needed to recover edit->source pairs from a Luminar catalog.db.
//
// CONFIDENCE: LOW. Luminar's catalog schema is not publicly documented by
// Skylum, has no plugin/export API that describes it, and no real
// catalog.db was available during this package's development to validate
// against. This query is a plausible reconstruction from indirect evidence
// only -- see docs/luminar-catalog.md for the full research trail and
// exactly what is and isn't backed by a source. Treat every identifier below
// (ZASSET, ZEDIT, ZFILEPATH, ...) as a hypothesis, not a fact.
//
// The table/column names follow Apple CoreData's SQLite store convention
// (Z-prefixed entity and attribute names, a synthetic Z_PK primary key) on
// the theory that Luminar's catalog originated as a CoreData-backed macOS
// app (Macphun-era Luminar) and kept its schema shape across the Windows
// port and the Neo rewrite -- consistent with, but not confirmed by,
// published accounts of the legacy Luminar 4 catalog.db format being a
// SQLite database associating original images with derived edit/state
// files.
//
// This is deliberately the ONLY place that schema knowledge lives.
// LoadQueryFile below lets an operator with a real catalog.db override this
// query without touching Go code or cutting a release -- the expected path
// to correcting it once someone can validate it against a real catalog
// (see the PR body / issue #6 acceptance criteria). `luminar-sync
// --dump-schema` is the companion tool for finding the real names.
//
// Columns, in order (EditSourcePairs requires exactly these four):
//   - source_path:    the original/master image's file path
//   - edit_path:      the edited/exported output's file path
//   - source_row_id:  the catalog's own identifier for the source row
//   - edit_row_id:    the catalog's own identifier for the edit row
//
// Only edits with a non-empty export path are returned -- an edit that
// exists only as in-catalog, non-destructive adjustment instructions (no
// exported file yet) has no second file on disk for branchDAM to link, since
// branchDAM's graph is a graph of files, not of in-app edit state.
const DefaultEditSourceQuery = `
SELECT
    asset.ZFILEPATH   AS source_path,
    edit.ZEXPORTPATH  AS edit_path,
    CAST(asset.Z_PK AS TEXT) AS source_row_id,
    CAST(edit.Z_PK  AS TEXT) AS edit_row_id
FROM ZEDIT edit
JOIN ZASSET asset ON asset.Z_PK = edit.ZASSET
WHERE edit.ZEXPORTPATH IS NOT NULL AND edit.ZEXPORTPATH != ''
`

// LoadQueryFile reads a SQL query from path, for --query-file. Returns the
// file's contents verbatim (trimmed of surrounding whitespace); no
// validation beyond that is done here -- EditSourcePairs itself validates
// the resulting column shape once the query actually runs.
func LoadQueryFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("luminar: read query file %s: %w", path, err)
	}
	q := strings.TrimSpace(string(data))
	if q == "" {
		return "", fmt.Errorf("luminar: query file %s is empty", path)
	}
	return q, nil
}
