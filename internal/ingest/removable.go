package ingest

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Removable-drive detection is how card detection works on Windows, which
// has no "/Volumes" or "/media/$USER" parent for cardRoots to name: every
// card gets its own drive letter. The Windows half (removable_windows.go)
// only asks the OS for the logical-drive bitmask and each drive's type;
// everything else -- turning the mask into drive roots, keeping only
// removable drives with media in them, merging them with the cardRoots
// volumes -- lives here, platform-neutral, so it is unit-tested on every
// OS. On other platforms listRemovableDrivesFn reports nothing and
// RemovableDriveDetection is false, so ListCardVolumes is exactly
// ListVolumesUnder there.

// driveRemovable is GetDriveType's DRIVE_REMOVABLE: SD/CF card readers and
// USB sticks. Fixed disks (DRIVE_FIXED, including most USB SSDs and hard
// drives -- the usual edit SSD and archive disks), network shares and
// optical drives are never treated as cards.
const driveRemovable = 2

// firstDetectedDrive skips A: and B:, the legacy floppy letters: Windows
// never assigns them to a card reader by default, and probing a real
// floppy drive on every poll would spin it up.
const firstDetectedDrive = 2

// removableDriveRoots returns the root (`E:\`) of every drive in mask (the
// GetLogicalDrives bitmask: bit 0 = A:) from C: on whose driveType is
// DRIVE_REMOVABLE and, when present is non-nil, that currently has media
// -- an empty card-reader slot keeps its drive letter and type with no
// card in it.
func removableDriveRoots(mask uint32, driveType func(root string) uint32, present func(root string) bool) []string {
	var out []string
	for i := firstDetectedDrive; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		if driveType(root) != driveRemovable {
			continue
		}
		if present != nil && !present(root) {
			continue
		}
		out = append(out, root)
	}
	return out
}

// driveRoot returns path's drive root (`E:\`, upper-cased) when path is a
// bare drive root -- `E:`, `E:\` or `e:/` -- and "" for anything else,
// including a folder on that drive. Parsed by hand so it behaves the same
// on every OS.
func driveRoot(path string) string {
	p := strings.TrimSpace(path)
	if len(p) < 2 || len(p) > 3 || p[1] != ':' {
		return ""
	}
	if c := p[0] | 0x20; c < 'a' || c > 'z' { // ASCII letter, either case
		return ""
	}
	if len(p) == 3 && p[2] != '\\' && p[2] != '/' {
		return ""
	}
	return strings.ToUpper(p[:1]) + `:\`
}

// RemovableDriveDetection reports whether this platform detects cards as
// removable drives (Windows) in addition to the volumes under cardRoots.
// When true, the card detector has something to watch even with no
// cardRoots configured.
const RemovableDriveDetection = removableDriveDetection

// listRemovableDrivesFn and driveTypeFn are the platform hooks
// (removable_windows.go / removable_other.go), indirected so tests can
// script a Windows drive layout on any OS.
var (
	listRemovableDrivesFn = listRemovableDrives
	driveTypeFn           = driveType
)

// IsRemovableDriveRoot reports whether path is the root of a drive the OS
// currently reports as removable (`E:\` for an SD card on Windows). Always
// false on platforms without removable-drive detection. The tray treats
// such a root as a card volume root for auto-eject, the way it treats an
// immediate subdirectory of a cardRoots parent elsewhere.
func IsRemovableDriveRoot(path string) bool {
	root := driveRoot(path)
	return root != "" && driveTypeFn(root) == driveRemovable
}

// ListCardVolumes is the full set of candidate card volumes: every volume
// ListVolumesUnder finds under cardRoots plus, on Windows, the root of
// every removable drive with media (honouring requireDCIM the same way),
// without duplicates. A cardRoots entry that is itself one of those
// removable drive roots is not walked as a parent: the drive is already a
// volume, and its subdirectories would be the same card again. It never
// returns an error: a failed drive listing is logged and the cardRoots
// volumes are still returned, since an error from a Detector's List ends
// its Watch loop for good.
func ListCardVolumes(cardRoots []string, requireDCIM bool) ([]string, error) {
	drives, err := listRemovableDrivesFn()
	if err != nil {
		slog.Debug("ingest: listing removable drives failed; using cardRoots only this poll", "err", err)
		return ListVolumesUnder(cardRoots, requireDCIM)
	}
	vols, err := ListVolumesUnder(withoutDriveRoots(cardRoots, drives), requireDCIM)
	if err != nil {
		return nil, err
	}
	return mergeVolumes(vols, drives, requireDCIM), nil
}

// withoutDriveRoots returns cardRoots minus any entry naming one of drives
// (`E:`, `E:\` or `e:/`, compared as drive roots).
func withoutDriveRoots(cardRoots, drives []string) []string {
	if len(drives) == 0 {
		return cardRoots
	}
	isDrive := make(map[string]bool, len(drives))
	for _, d := range drives {
		if r := driveRoot(d); r != "" {
			isDrive[r] = true
		}
	}
	out := make([]string, 0, len(cardRoots))
	for _, root := range cardRoots {
		if r := driveRoot(root); r != "" && isDrive[r] {
			continue
		}
		out = append(out, root)
	}
	return out
}

// mergeVolumes appends each drive root not already in vols (compared
// case-insensitively, as drive letters are) and, when requireDCIM is set,
// holding a DCIM directory.
func mergeVolumes(vols, drives []string, requireDCIM bool) []string {
	seen := make(map[string]bool, len(vols))
	for _, v := range vols {
		seen[strings.ToUpper(filepath.Clean(v))] = true
	}
	for _, d := range drives {
		key := strings.ToUpper(filepath.Clean(d))
		if seen[key] {
			continue
		}
		if requireDCIM {
			st, err := os.Stat(filepath.Join(d, "DCIM"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				// A permission or device error reads as "no DCIM" (the
				// drive is skipped), as in ListVolumesUnder; say why.
				slog.Debug("ingest: cannot check removable drive for DCIM; skipping it", "drive", d, "err", err)
			}
			if err != nil || !st.IsDir() {
				continue
			}
		}
		seen[key] = true
		vols = append(vols, d)
	}
	return vols
}
