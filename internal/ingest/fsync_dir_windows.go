//go:build windows

package ingest

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// syncParentDir opens dir read-only and flushes its metadata, ensuring the
// directory entry (the file we just wrote into it) is durable on power loss.
// Without this, a crash between the file's own fsync and the OS flushing the
// parent directory's metadata can silently lose the file.
func syncParentDir(dir string) error {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("encode path %s: %w", dir, err)
	}
	h, err := windows.CreateFile(
		path,
		windows.FILE_GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("open parent dir %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.FlushFileBuffers(h); err != nil {
		return fmt.Errorf("flush parent dir %s: %w", dir, err)
	}
	return nil
}
