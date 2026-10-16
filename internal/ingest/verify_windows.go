//go:build windows

package ingest

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// openForVerify on Windows opens path with FILE_FLAG_NO_BUFFERING via
// CreateFile, bypassing the system file cache so the re-read comes from the
// device (or, on SMB, from the server) rather than from the pages the write
// just filled. Reads then go through copyAligned's 4 KiB-aligned,
// sector-multiple buffer, as unbuffered I/O requires.
//
// The buffered floor (plain os.Open, VerifyMethodBufferedFloor) is used
// only when CreateFile's error says unbuffered I/O is unsupported for this
// path (windowsUnbufferedUnsupported: ERROR_INVALID_PARAMETER,
// ERROR_NOT_SUPPORTED, ERROR_INVALID_FUNCTION). Any other error -- missing
// file, access denied, sharing violation -- is returned, so Verify fails
// rather than quietly degrading (AGENTS.md invariant 2). The decision is
// made once, here, before any byte is read; never mid-stream.
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, "", err
	}

	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_SEQUENTIAL_SCAN,
		0,
	)
	if err == nil {
		f := os.NewFile(uintptr(handle), path)
		return f, VerifyMethodUnbuffered, nil
	}
	if !windowsUnbufferedUnsupported(err) {
		return nil, "", &os.PathError{Op: "open (unbuffered)", Path: path, Err: err}
	}

	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}
	return f, VerifyMethodBufferedFloor, nil
}
