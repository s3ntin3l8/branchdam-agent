package ingest

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestWindowsUnbufferedUnsupported pins which CreateFile errors let the
// Windows verify fall back to the buffered floor: only the three that mean
// FILE_FLAG_NO_BUFFERING is unsupported, wrapped or not. Everything else --
// including the errors a damaged or vanished destination produces -- must
// surface (AGENTS.md invariant 2).
func TestWindowsUnbufferedUnsupported(t *testing.T) {
	const (
		errorFileNotFound     syscall.Errno = 2
		errorAccessDenied     syscall.Errno = 5
		errorNotReady         syscall.Errno = 21
		errorSharingViolation syscall.Errno = 32
	)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"ERROR_INVALID_PARAMETER", syscall.Errno(87), true},
		{"ERROR_NOT_SUPPORTED", syscall.Errno(50), true},
		{"ERROR_INVALID_FUNCTION", syscall.Errno(1), true},
		{"wrapped in PathError", &os.PathError{Op: "open", Path: `E:\x`, Err: syscall.Errno(87)}, true},
		{"wrapped with %w", fmt.Errorf("createfile: %w", syscall.Errno(50)), true},
		{"ERROR_FILE_NOT_FOUND", errorFileNotFound, false},
		{"ERROR_ACCESS_DENIED", errorAccessDenied, false},
		{"ERROR_NOT_READY", errorNotReady, false},
		{"ERROR_SHARING_VIOLATION", errorSharingViolation, false},
		{"not an errno", errors.New("invalid parameter"), false},
		{"nil", nil, false},
	} {
		if got := windowsUnbufferedUnsupported(tc.err); got != tc.want {
			t.Errorf("%s: windowsUnbufferedUnsupported = %v, want %v", tc.name, got, tc.want)
		}
	}
}
