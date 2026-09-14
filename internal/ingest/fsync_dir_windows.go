//go:build windows

package ingest

// syncParentDir is intentionally a no-op on Windows. FlushFileBuffers does not
// support directory handles (even when opened with FILE_FLAG_BACKUP_SEMANTICS)
// and returns ERROR_ACCESS_DENIED on supported NTFS volumes. The destination
// files themselves have already been flushed above this call; NTFS journals
// directory-entry updates transactionally.
func syncParentDir(dir string) error {
	_ = dir
	return nil
}
