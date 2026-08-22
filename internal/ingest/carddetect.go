package ingest

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// DefaultPollInterval matches the plan/issue's stated "a 2s poll is
// sufficient" -- no per-OS kernel watching (fsnotify-style) in this PR;
// that is explicitly deferred (issue #2's "Out of scope" section).
const DefaultPollInterval = 2 * time.Second

// ListVolumesFunc lists the currently-present candidate removable-volume
// roots -- one path per mounted card. Injected so Detector is testable
// without real removable media: production code (ListVolumesUnder) walks a
// fixed set of parent directories; a test substitutes a fake that returns a
// scripted sequence.
type ListVolumesFunc func() ([]string, error)

// Detector polls List every Interval and reports which candidate roots
// appeared or disappeared since the previous poll, by diffing two sets --
// no OS-level mount/unmount event API, matching the plan's "poll-based is
// enough for v1."
type Detector struct {
	List     ListVolumesFunc
	Interval time.Duration
}

// NewDetector builds a Detector over cardRoots (parent directories to scan
// for currently-mounted subdirectories, e.g. "/media/$USER" on Linux,
// "/Volumes" on macOS) using ListVolumesUnder, polling every interval
// (DefaultPollInterval if <= 0).
func NewDetector(cardRoots []string, interval time.Duration) *Detector {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	return &Detector{
		List:     func() ([]string, error) { return ListVolumesUnder(cardRoots) },
		Interval: interval,
	}
}

// ListVolumesUnder returns every immediate subdirectory of every root in
// cardRoots that currently exists -- the heuristic this PR ships for "is
// this a removable volume": any mounted directory under one of the
// configured parent roots. This is intentionally simple (no DCIM-folder
// sniffing, no filesystem-type check) -- see the plan doc's M1 section,
// which scopes card detection to "poll for removable volumes," not to
// distinguishing a card from any other mount. A root that doesn't exist
// (not an error -- e.g. no card reader plugged in at all means "/media/x"
// itself may not exist yet) contributes no entries rather than failing the
// whole call.
func ListVolumesUnder(cardRoots []string) ([]string, error) {
	var out []string
	for _, root := range cardRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
	}
	return out, nil
}

// Diff is one poll's result: volumes present now that weren't in the
// previous poll (Inserted) and volumes that were present but have vanished
// (Removed).
type Diff struct {
	Inserted []string
	Removed  []string
}

// Watch runs Detector's poll loop until ctx is cancelled, calling onDiff
// once per tick with that tick's Diff (skipped when both Inserted and
// Removed are empty). Returns ctx.Err() on cancellation, or the first error
// List returns (List returning an error is not expected in practice --
// ListVolumesUnder itself never returns one -- but a custom List a caller
// injects might).
func (d *Detector) Watch(ctx context.Context, onDiff func(Diff)) error {
	prev := map[string]bool{}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()

	poll := func() error {
		cur, err := d.List()
		if err != nil {
			return err
		}
		curSet := make(map[string]bool, len(cur))
		var diff Diff
		for _, v := range cur {
			curSet[v] = true
			if !prev[v] {
				diff.Inserted = append(diff.Inserted, v)
			}
		}
		for v := range prev {
			if !curSet[v] {
				diff.Removed = append(diff.Removed, v)
			}
		}
		prev = curSet
		if len(diff.Inserted) > 0 || len(diff.Removed) > 0 {
			onDiff(diff)
		}
		return nil
	}

	if err := poll(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := poll(); err != nil {
				return err
			}
		}
	}
}
