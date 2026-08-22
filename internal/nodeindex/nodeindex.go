// Package nodeindex maps a workstation file path to the branchDAM nodeUuid
// that path was ingested as.
//
// This exists because of a real gap in branchDAM's agent contract (plan gap
// 5, "Contract gaps to design around"): there is no agent-reachable
// lookup-by-path endpoint. The only node identities an agent can ever know
// are ones it minted itself at ingest time -- for anything else, resolving a
// path to a nodeUuid would mean reimplementing storage.Guard.Resolve and
// pipeline.Commit's move/collision logic client-side, which is exactly the
// workaround issue #6 says not to build.
//
// Resolver is the seam: v1 (this package's FileIndex) is a hand-editable or
// script-generated JSON file, since branchdam-agent's M1/M2 milestones (the
// SD-card ingest core and its offline queue.db, which are the eventual real
// source of this mapping -- see the plan's M2 section) had not landed when
// this package was written. Once queue.db exists, it becomes a second
// Resolver implementation with the same one-method interface; nothing above
// this package needs to change.
package nodeindex

import (
	"encoding/json"
	"fmt"
	"os"
)

// Resolver maps a file path to the nodeUuid it was ingested as. ok is false
// if path is not in the index (not an error -- an unresolvable path is an
// expected, common case, not a failure of the index itself).
type Resolver interface {
	Resolve(path string) (nodeUUID string, ok bool, err error)
}

// FileIndex is a Resolver backed by a JSON file: a flat object mapping
// absolute workstation file paths to nodeUuid strings, e.g.:
//
//	{
//	  "/mnt/card/DCIM/100MSDCF/IMG_0001.ARW": "0198f2c1-2e3a-7c9e-8b1a-1234567890ab"
//	}
//
// The whole file is loaded into memory once at Load time; Resolve is a plain
// map lookup and never errors on its own (Load already surfaced any I/O or
// parse failure).
type FileIndex struct {
	byPath map[string]string
}

// Load reads and parses path as a FileIndex. A missing file is an error, not
// an empty index -- callers that want an empty index (e.g. a dry run with no
// mapping yet) should pass an explicit empty JSON object ("{}") rather than
// omitting the flag, so a typo'd path fails loudly instead of silently
// resolving nothing.
func Load(path string) (*FileIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nodeindex: read %s: %w", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("nodeindex: parse %s: %w", path, err)
	}
	return &FileIndex{byPath: m}, nil
}

// Resolve looks path up verbatim (no normalization, no symlink resolution --
// the caller is expected to pass the same absolute path form the index was
// built with, matching how EditSourcePair.SourcePath/EditPath are read
// directly from the catalog with no rewrite pass applied). Never errors.
func (f *FileIndex) Resolve(path string) (string, bool, error) {
	uuid, ok := f.byPath[path]
	return uuid, ok, nil
}

// Len reports how many path->nodeUuid entries the index holds.
func (f *FileIndex) Len() int {
	return len(f.byPath)
}
