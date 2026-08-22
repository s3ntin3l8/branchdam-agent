//go:build windows

package ingest

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// openForVerify on Windows uses FILE_FLAG_NO_BUFFERING via CreateFile to
// bypass the OS page cache. If CreateFile with FILE_FLAG_NO_BUFFERING fails
// (e.g. sector alignment or unsupported filesystem), it falls back to a plain
// os.Open returning VerifyMethodBufferedFloor.
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

	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}
	return f, VerifyMethodBufferedFloor, nil
}
