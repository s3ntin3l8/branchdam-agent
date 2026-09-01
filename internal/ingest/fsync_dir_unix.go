//go:build linux || darwin

package ingest

import (
	"fmt"
	"os"
)

// syncParentDir opens dir read-only and fsyncs it, ensuring the directory
// entry (the file we just wrote into it) is durable on power loss. Without
// this, a crash between the file's own fsync and the OS flushing the parent
// directory's metadata can silently lose the file.
func syncParentDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent dir %s: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync parent dir %s: %w", dir, err)
	}
	return nil
}
