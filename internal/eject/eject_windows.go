//go:build windows

package eject

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// Eject safely ejects the removable volume holding mountPath (`E:\` or any
// path on it): it opens the volume, then locks it (FSCTL_LOCK_VOLUME,
// retried while another process briefly holds a file open), dismounts it
// (FSCTL_DISMOUNT_VOLUME), clears any media-removal lock
// (IOCTL_STORAGE_MEDIA_REMOVAL, PreventMediaRemoval=FALSE) and ejects the
// media (IOCTL_STORAGE_EJECT_MEDIA) -- Microsoft's documented sequence
// (KB165721). Ejecting without the lock and dismount, as this used to,
// could pull the media out from under a process with a file still open
// and leave the file system's cached state unflushed.
//
// A failure is a *StepError naming the step that failed; see its doc
// comment for which failures still leave the card safe to pull. Closing
// the handle (deferred) releases the lock in every case.
func Eject(mountPath string) error {
	volPath, err := volumeDevicePath(mountPath)
	if err != nil {
		return &StepError{Path: mountPath, Step: StepOpen, Err: err}
	}
	pathPtr, err := windows.UTF16PtrFromString(volPath)
	if err != nil {
		return &StepError{Path: mountPath, Step: StepOpen, Err: err}
	}

	// FSCTL_LOCK_VOLUME needs read or write access on the volume handle.
	// Read/write first; read-only as a fallback (a write-protected card
	// can refuse a writable open), never the zero-access handle the old
	// code fell back to, which can't lock anything.
	handle, err := openVolume(pathPtr, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		handle, err = openVolume(pathPtr, windows.GENERIC_READ)
		if err != nil {
			return &StepError{Path: mountPath, Step: StepOpen, Err: fmt.Errorf("%s: %w", volPath, err)}
		}
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	ctl := func(code uint32, in []byte) error {
		var inPtr *byte
		if len(in) > 0 {
			inPtr = &in[0]
		}
		var returned uint32
		return windows.DeviceIoControl(handle, code, inPtr, uint32(len(in)), nil, 0, &returned, nil)
	}
	return runEjectSequence(mountPath, ctl, time.Sleep)
}

func openVolume(path *uint16, access uint32) (windows.Handle, error) {
	return windows.CreateFile(
		path,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
}
