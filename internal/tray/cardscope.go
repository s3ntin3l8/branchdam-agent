package tray

import (
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/agentlog"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// This file decides which paths count as camera-card volumes -- the only
// things the tray may ingest without the operator naming them, and the only
// things it may ever eject. cardRoots (ingest.cardRoots, Runner.watchDirs)
// are the PARENTS volumes mount under ("/Volumes", "/media/$USER"); the
// volumes are their immediate subdirectories, and those parents also hold
// every other mounted disk on the workstation, including -- quite commonly
// -- the NAS share the archive lives on and the SSD the edit copies go to.

// Platform hooks for card volumes, indirected so tests can script a
// Windows removable-drive layout on any OS. On Windows every card is its
// own removable drive (`E:\`) rather than a subdirectory of a cardRoots
// parent, so listCardVolumes adds those drives and isRemovableDriveRoot
// lets their roots count as card volume roots; elsewhere both reduce to
// the cardRoots behaviour (ingest.RemovableDriveDetection is false).
var (
	listCardVolumes         = ingest.ListCardVolumes
	isRemovableDriveRoot    = ingest.IsRemovableDriveRoot
	removableDriveDetection = ingest.RemovableDriveDetection
)

// selectCardVolumes lists the volumes currently mounted under cardRoots,
// plus every removable drive with media on Windows (ingest.ListCardVolumes,
// honouring requireDCIM), and drops any volume that contains, or is
// contained in, one of the destination roots. kept is what "Ingest now"
// walks and what the card detector reports; excluded is returned only so a
// one-shot caller can say why a disk was left out (the detector polls
// every 2s and stays quiet).
func selectCardVolumes(cardRoots []string, requireDCIM bool, destRoots ...string) (kept, excluded []string) {
	vols, err := listCardVolumes(cardRoots, requireDCIM)
	if err != nil {
		slog.Warn("tray: list card volumes failed", "err", agentlog.Sanitize(err.Error()))
		return nil, nil
	}
	return excludeDestinationVolumes(vols, destRoots...)
}

// excludeDestinationVolumes splits vols into those that overlap (contain,
// equal or sit inside) one of destRoots and those that don't. Empty
// destRoots entries are ignored.
func excludeDestinationVolumes(vols []string, destRoots ...string) (kept, excluded []string) {
	kept = make([]string, 0, len(vols))
	for _, v := range vols {
		if overlapsAny(v, destRoots) {
			excluded = append(excluded, v)
			continue
		}
		kept = append(kept, v)
	}
	return kept, excluded
}

// isCardVolumeRoot reports whether path is exactly a volume root the
// detector can report: an immediate subdirectory of one of cardRoots, or
// (Windows) the root of a removable drive, that does not overlap any of
// destRoots. It is the structural half of the auto-eject decision -- a
// folder picked with "Import from folder…", a path posted to
// /api/actions/ingest, a folder on a card, or a cardRoots parent itself
// never qualifies, however its ingest went.
func isCardVolumeRoot(path string, cardRoots []string, destRoots ...string) bool {
	if path == "" {
		return false
	}
	if isRemovableDriveRoot(path) {
		return !overlapsAny(filepath.Clean(path), destRoots)
	}
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	if parent == clean { // filesystem root
		return false
	}
	underRoot := false
	for _, root := range cardRoots {
		if root != "" && samePath(parent, filepath.Clean(root)) {
			underRoot = true
			break
		}
	}
	return underRoot && !overlapsAny(clean, destRoots)
}

// IsCardVolumeRoot is isCardVolumeRoot for callers outside the tray -- the
// headless `ingest` subcommand gates its own auto-eject on the same
// structural check, so `ingest -card /Volumes/NAS` can never unmount the
// archive share.
func IsCardVolumeRoot(path string, cardRoots []string, destRoots ...string) bool {
	return isCardVolumeRoot(path, cardRoots, destRoots...)
}

// overlapsAny reports whether path equals, contains, or is contained in any
// non-empty entry of roots. Both sides are compared as written and with
// symlinks resolved (when resolvable), so an archiveRoot configured through
// a symlink (~/NAS -> /Volumes/NAS) still excludes the volume behind it.
func overlapsAny(path string, roots []string) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, p := range pathForms(path) {
			for _, r := range pathForms(root) {
				if isWithin(p, r) || isWithin(r, p) {
					return true
				}
			}
		}
	}
	return false
}

// pathForms returns the cleaned path plus its symlink-resolved form when
// that differs (and can be resolved -- an unmounted path can't be). When a
// destination root is configured through a symlink whose target is not
// mounted, only its lexical form is compared; that volume is then not
// mounted either, so there is nothing for the check to exclude.
func pathForms(p string) []string {
	clean := filepath.Clean(p)
	forms := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		forms = append(forms, resolved)
	}
	return forms
}

// isWithin reports whether child is parent or lies beneath it. Both must
// already be cleaned.
func isWithin(child, parent string) bool {
	if samePath(child, parent) {
		return true
	}
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if caseInsensitivePaths() {
		return len(child) > len(prefix) && strings.EqualFold(child[:len(prefix)], prefix)
	}
	return strings.HasPrefix(child, prefix)
}

// volumeName is the short name shown for a card volume in the ingest
// confirmation dialog and notifications: its last path element
// ("EOS_DIGITAL" for /Volumes/EOS_DIGITAL), or the path itself when it has
// none -- a Windows drive root (`E:\`), whose filepath.Base is just `\`.
func volumeName(path string) string {
	base := filepath.Base(path)
	if base == "" || base == "." || base == "/" || base == `\` {
		return path
	}
	return base
}

// caseInsensitivePaths reports whether the default filesystems on this OS
// compare names case-insensitively (APFS/HFS+ default, NTFS). Erring toward
// "overlaps" there only ever excludes more volumes, never fewer.
func caseInsensitivePaths() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}
