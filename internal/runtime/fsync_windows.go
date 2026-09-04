//go:build windows

package runtime

// fsyncDir is a no-op on Windows: the Win32 FlushFileBuffers
// function returns ERROR_INVALID_HANDLE when called on a directory
// handle, not because the flush itself failed but because
// directory-handle flushing is not part of the Win32 surface. NTFS
// and ReFS commit directory entries transactionally through the
// journal, so a power loss between the in-app rename and the
// journal commit recovers on the next NTFS chkdsk the same way a
// pre-rename state would. The in-memory carry-forward (PR #148)
// is the primary correctness layer; the cross-session half this
// dir-fsync is meant to defend simply relies on NTFS's own atomic
// write semantics, which are stronger than POSIX rename in this
// respect.
//
// Returning a non-nil error here would cause runtime.Save to
// warn-log a non-actionable "could not fsync parent directory"
// on every successful drain pass on Windows, so we return nil.
func fsyncDir(dir string) error {
	_ = dir
	return nil
}
