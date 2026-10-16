package ingest

import (
	"errors"
	"syscall"
)

// Windows error codes CreateFile returns when the file system, redirector
// or device behind a path cannot open it with FILE_FLAG_NO_BUFFERING.
// Spelled out as raw syscall.Errno values (not golang.org/x/sys/windows
// constants) so the classification below compiles and is tested on every
// OS, while only verify_windows.go ever calls it.
const (
	winErrorInvalidFunction  syscall.Errno = 1  // ERROR_INVALID_FUNCTION: driver doesn't implement the flag (some network redirectors)
	winErrorNotSupported     syscall.Errno = 50 // ERROR_NOT_SUPPORTED: request not supported by the file system or device
	winErrorInvalidParameter syscall.Errno = 87 // ERROR_INVALID_PARAMETER: flag combination refused, e.g. sector alignment the device can't honour
)

// windowsUnbufferedUnsupported reports whether err, from a CreateFile with
// FILE_FLAG_NO_BUFFERING, means "unbuffered I/O is unsupported here" -- the
// only case in which openForVerify may fall back to the buffered floor
// (AGENTS.md invariant 2). Any other error -- the file is missing, access
// is denied, it is locked by another process, the device is not ready --
// says something is wrong with the file just written, and falling back
// would hide it behind a verify that reads the same unhealthy path again
// (or, worse, the page cache), so it is surfaced instead. This mirrors
// verify_linux.go, which falls back only on EINVAL/EOPNOTSUPP from
// O_DIRECT.
func windowsUnbufferedUnsupported(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case winErrorInvalidFunction, winErrorNotSupported, winErrorInvalidParameter:
		return true
	default:
		return false
	}
}
