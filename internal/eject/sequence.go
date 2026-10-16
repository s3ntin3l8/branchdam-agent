package eject

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the platform-neutral half of the Windows eject: the
// control codes, the order they are issued in, the lock retry policy and
// the error reporting. eject_windows.go only opens the volume handle and
// forwards each step to DeviceIoControl, so everything below is unit-tested
// on every OS (sequence_test.go) even though it only runs on Windows.

// ctlCode is the Windows SDK's CTL_CODE macro.
func ctlCode(deviceType, function, method, access uint32) uint32 {
	return deviceType<<16 | access<<14 | function<<2 | method
}

const (
	fileDeviceFileSystem = 0x00000009 // FILE_DEVICE_FILE_SYSTEM
	ioctlStorageBase     = 0x0000002d // IOCTL_STORAGE_BASE (FILE_DEVICE_MASS_STORAGE)
	methodBuffered       = 0          // METHOD_BUFFERED
	fileAnyAccess        = 0          // FILE_ANY_ACCESS
	fileReadAccess       = 1          // FILE_READ_ACCESS
)

// Control codes for the eject sequence. x/sys/windows defines none of them.
var (
	// FSCTL_LOCK_VOLUME: exclusive lock; fails while any file on the volume
	// is open. Taking it also flushes the volume.
	fsctlLockVolume = ctlCode(fileDeviceFileSystem, 6, methodBuffered, fileAnyAccess) // 0x00090018
	// FSCTL_DISMOUNT_VOLUME: invalidate every cached handle and detach the
	// file system, so nothing can write to the card once it is pulled.
	fsctlDismountVolume = ctlCode(fileDeviceFileSystem, 8, methodBuffered, fileAnyAccess) // 0x00090020
	// IOCTL_STORAGE_MEDIA_REMOVAL with PreventMediaRemoval=FALSE: clear any
	// removal lock a driver or earlier caller left, or the eject is refused.
	ioctlStorageMediaRemoval = ctlCode(ioctlStorageBase, 0x0201, methodBuffered, fileReadAccess) // 0x002D4804
	// IOCTL_STORAGE_EJECT_MEDIA: eject (or, for a card reader, power down)
	// the media.
	ioctlStorageEjectMedia = ctlCode(ioctlStorageBase, 0x0202, methodBuffered, fileReadAccess) // 0x002D4808
)

// Lock retry policy, from Microsoft's own eject sample (KB165721): the
// lock fails while anything still has a file on the card open -- Explorer
// thumbnailing a freshly written folder, an antivirus scan -- which
// usually clears within seconds.
const (
	lockRetries    = 20
	lockRetryDelay = 500 * time.Millisecond
)

// Step names reported in StepError.
const (
	StepOpen         = "open volume"
	StepLock         = "lock volume"
	StepDismount     = "dismount volume"
	StepMediaRemoval = "allow media removal"
	StepEject        = "eject media"
)

// StepError reports which step of an eject failed. Steps before it
// completed; none after it ran. A failure at StepMediaRemoval or StepEject
// means the volume was already locked and dismounted -- the card is safe
// to pull by hand (Dismounted() is true), it just was not ejected.
type StepError struct {
	Path string
	Step string
	Err  error
}

func (e *StepError) Error() string {
	msg := fmt.Sprintf("eject %s: %s: %v", e.Path, e.Step, e.Err)
	switch {
	case e.Step == StepLock:
		msg += " (a file on the card is probably still open -- close it and eject again)"
	case e.Dismounted():
		msg += " (the volume is dismounted and safe to remove, but was not ejected)"
	}
	return msg
}

func (e *StepError) Unwrap() error { return e.Err }

// Dismounted reports whether the file system was already dismounted when
// the step failed.
func (e *StepError) Dismounted() bool {
	return e.Step == StepMediaRemoval || e.Step == StepEject
}

// volumeControl issues one DeviceIoControl on an open volume handle.
type volumeControl func(code uint32, in []byte) error

// runEjectSequence locks, dismounts, clears the removal lock on and ejects
// the volume behind ctl, in that order, stopping at the first failure. The
// lock is retried (lockRetries x lockRetryDelay, via sleep) because it
// fails transiently while another process briefly holds a file open;
// nothing else is retried. The caller closes the handle afterwards, which
// is also what releases the lock.
func runEjectSequence(path string, ctl volumeControl, sleep func(time.Duration)) error {
	var lockErr error
	for attempt := 0; attempt < lockRetries; attempt++ {
		if lockErr = ctl(fsctlLockVolume, nil); lockErr == nil {
			break
		}
		if attempt < lockRetries-1 {
			sleep(lockRetryDelay)
		}
	}
	if lockErr != nil {
		return &StepError{Path: path, Step: StepLock, Err: lockErr}
	}
	if err := ctl(fsctlDismountVolume, nil); err != nil {
		return &StepError{Path: path, Step: StepDismount, Err: err}
	}
	// PREVENT_MEDIA_REMOVAL is a single BOOLEAN; 0 = allow removal.
	if err := ctl(ioctlStorageMediaRemoval, []byte{0}); err != nil {
		return &StepError{Path: path, Step: StepMediaRemoval, Err: err}
	}
	if err := ctl(ioctlStorageEjectMedia, nil); err != nil {
		return &StepError{Path: path, Step: StepEject, Err: err}
	}
	return nil
}

// errNoDriveLetter is volumeDevicePath's refusal for anything that is not a
// drive-letter path or an already-formed \\.\X: device path -- a UNC share
// or a folder mounted without a letter cannot be ejected this way.
var errNoDriveLetter = errors.New("no drive letter found")

// volumeDevicePath turns a Windows mount path (`E:\`, `E:`, `E:\DCIM\100`,
// or an already-formed `\\.\E:`) into the volume device path CreateFile
// needs to open the whole volume (`\\.\E:`). Parsed by hand rather than with
// filepath.VolumeName so it behaves identically -- and is tested -- on
// every OS.
func volumeDevicePath(mountPath string) (string, error) {
	p := strings.TrimSpace(mountPath)
	if rest, ok := strings.CutPrefix(p, `\\.\`); ok {
		p = rest
	}
	if len(p) < 2 || p[1] != ':' || !isASCIILetter(p[0]) {
		return "", fmt.Errorf("%w in %q", errNoDriveLetter, mountPath)
	}
	if len(p) > 2 && p[2] != '\\' && p[2] != '/' {
		return "", fmt.Errorf("%w in %q", errNoDriveLetter, mountPath)
	}
	return `\\.\` + strings.ToUpper(p[:1]) + ":", nil
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
