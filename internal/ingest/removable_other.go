//go:build !windows

package ingest

// Removable-drive detection is Windows-only: on Linux and macOS every
// card mounts under a cardRoots parent ("/media/$USER", "/Volumes"), which
// ListVolumesUnder already covers.
const removableDriveDetection = false

func listRemovableDrives() ([]string, error) { return nil, nil }

func driveType(string) uint32 { return 0 }
