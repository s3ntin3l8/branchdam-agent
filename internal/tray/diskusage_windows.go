//go:build windows

package tray

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// diskUsage returns free/total bytes for the volume containing path, via
// GetDiskFreeSpaceEx. freeBytesAvailableToCaller (not totalNumberOfFreeBytes)
// is what "free" reports -- the two can differ under a per-user disk quota,
// and the caller-visible figure is the one that actually bounds what this
// agent can write.
func diskUsage(path string) (free, total uint64, err error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	var freeAvailableToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeAvailableToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: err}
	}
	return freeAvailableToCaller, totalBytes, nil
}
