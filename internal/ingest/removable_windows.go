//go:build windows

package ingest

import (
	"os"

	"golang.org/x/sys/windows"
)

const removableDriveDetection = true

// listRemovableDrives returns the root of every removable drive (SD/CF
// card reader slot, USB stick) that currently has media in it. Called on
// every detector poll (2s by default): GetLogicalDrives and GetDriveType
// are cheap and never touch the media, and the media check is one stat of
// the drive root, which on an empty slot fails at once with
// ERROR_NOT_READY. The Go runtime runs with SEM_FAILCRITICALERRORS set, so
// probing an empty slot never pops the "insert a disk" dialog.
func listRemovableDrives() ([]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	return removableDriveRoots(mask, driveType, mediaPresent), nil
}

// driveType wraps GetDriveType for a drive root like `E:\`.
func driveType(root string) uint32 {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	return windows.GetDriveType(p)
}

// mediaPresent reports whether root has a readable file system mounted --
// false for a card-reader slot with no card in it.
func mediaPresent(root string) bool {
	_, err := os.Stat(root)
	return err == nil
}
