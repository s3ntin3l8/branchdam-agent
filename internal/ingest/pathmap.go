package ingest

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// ErrNoPathMapping is returned when a written archive path has no
// configured PathMapping whose WorkstationPath prefixes it -- the agent has
// no way to construct the server-container, absolute, symlink-free path
// NodeCreatedPayload.FilePath requires (there is no server-side rewrite
// pass on agent payloads, unlike cfg.PathRewrites -- see internal/config's
// doc comment). Sending a raw workstation path in that situation would hit
// ErrUnknownLocation server-side, burn three retries, and land FAILED with
// no feedback channel -- surfacing this locally, before any POST, is
// strictly better.
var ErrNoPathMapping = errors.New("ingest: no pathMapping covers this workstation path")

// ToContainerPath translates workstationPath into its server-container
// equivalent using the longest-prefix-matching entry in mappings. Longest
// match wins so a more specific mapping (e.g. a per-camera subtree) can
// override a broader one covering the same archive root.
func ToContainerPath(mappings []config.PathMapping, workstationPath string) (string, error) {
	clean := filepath.ToSlash(workstationPath)
	best := -1
	bestLen := -1
	for i, m := range mappings {
		// Trim a trailing slash once, before both the equality and prefix
		// checks -- comparing the equality branch against the untrimmed
		// WorkstationPath was a real bug (caught during self-review): a
		// configured mapping of "/mnt/nas/archive/" (trailing slash) would
		// never equality-match a lookup of the bare root "/mnt/nas/archive"
		// (no trailing slash), wrongly falling through to ErrNoPathMapping
		// for a file landing exactly at the mapping's root.
		wp := strings.TrimRight(filepath.ToSlash(m.WorkstationPath), "/")
		if wp == "" {
			continue
		}
		if clean == wp || strings.HasPrefix(clean, wp+"/") {
			if len(wp) > bestLen {
				best = i
				bestLen = len(wp)
			}
		}
	}
	if best == -1 {
		return "", fmt.Errorf("%w: %s", ErrNoPathMapping, workstationPath)
	}

	m := mappings[best]
	wp := strings.TrimRight(filepath.ToSlash(m.WorkstationPath), "/")
	rel := strings.TrimPrefix(clean, wp)
	rel = strings.TrimPrefix(rel, "/")
	cp := strings.TrimRight(filepath.ToSlash(m.ContainerPath), "/")
	if rel == "" {
		return cp, nil
	}
	return cp + "/" + rel, nil
}
