package ingest

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

// syncParentDirFn is indirected through a package var so tests can substitute
// a recording stub (running fsync in a unit test on a temp dir is fine, but
// the indirection lets us verify the call paths and log semantics without
// needing power-loss simulation).
var syncParentDirFn = syncParentDir

// WriteResult is what DualWrite produces: the two destination paths it
// wrote, the source's size, and the two hashes computed in the same pass
// (fullHash is BLAKE3-256 over the whole stream -- what gets sent to the
// server as fullHash and what Verify re-checks; fastHash is the sampled
// xxHash64 digest, byte-identical to hashing.FastHash's ReaderAt-based
// algorithm via hashing.StreamingFastHasher).
type WriteResult struct {
	ArchivePath string
	LocalPath   string
	SizeBytes   int64
	FullHash    string
	FastHash    string
}

// DualWrite reads srcPath exactly once and streams it simultaneously into
// archivePath and localPath (parent directories created as needed),
// computing BLAKE3-256 (full_hash) and capturing FastHash's three sample
// windows (fast_hash) as bytes pass -- no second read of the source. This
// is the "one read, two writes" half of issue #2's contract; Verify (in
// verify.go) is the separate cache-defeating re-read that proves both
// copies actually landed correctly.
//
// Both destination files are created with O_EXCL: DualWrite never
// overwrites an existing file at either destination, since a silent
// overwrite of an already-archived master would be exactly the kind of
// data-loss bug a verified-write pipeline exists to prevent. Any failure
// partway through -- the second destination's create failing after the
// first succeeded, a mid-copy I/O error, a source-size mismatch, a failed
// fsync/close -- removes every destination file this call itself created
// before returning the error: a failed ingest must never leave a partial or
// corrupt file sitting in the archive location that could later be mistaken
// for a real, complete master (by a human, or by a concurrent server-side
// scan of the same directory). A caller retrying after a DIFFERENT kind of
// failure (e.g. the process was killed before DualWrite could clean up)
// must still remove or rename any surviving partial destination itself
// before retrying -- O_EXCL means a stray leftover file blocks the retry
// with "file exists" rather than silently overwriting it, which is the
// safer failure mode of the two.
func DualWrite(srcPath, archivePath, localPath string, opts ...WriteOption) (result WriteResult, err error) {
	var created []string
	defer func() {
		if err != nil {
			for _, p := range created {
				_ = os.Remove(p)
			}
		}
	}()

	src, err := os.Open(srcPath) //nolint:gosec // srcPath is operator-supplied (card contents), not attacker input
	if err != nil {
		return WriteResult{}, fmt.Errorf("ingest: open source %s: %w", srcPath, err)
	}
	defer func() { _ = src.Close() }()

	info, err := src.Stat()
	if err != nil {
		return WriteResult{}, fmt.Errorf("ingest: stat source %s: %w", srcPath, err)
	}
	size := info.Size()

	archiveFile, err := createExclusive(archivePath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("ingest: create archive dest %s: %w", archivePath, err)
	}
	created = append(created, archivePath)
	defer func() { _ = archiveFile.Close() }()

	localFile, err := createExclusive(localPath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("ingest: create local dest %s: %w", localPath, err)
	}
	created = append(created, localPath)
	defer func() { _ = localFile.Close() }()

	fullHasher := blake3.New()
	fastHasher := hashing.NewStreamingFastHasher(size)

	writers := []io.Writer{archiveFile, localFile, fullHasher, fastHasher}
	if o := applyWriteOptions(opts); o.onBytes != nil {
		writers = append(writers, &progressWriter{onBytes: o.onBytes})
	}
	mw := io.MultiWriter(writers...)
	n, copyErr := io.Copy(mw, src)
	if copyErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: copy %s: %w", srcPath, copyErr)
	}
	if n != size {
		return WriteResult{}, fmt.Errorf("ingest: copy %s: read %d bytes, stat reported %d (source changed mid-copy)", srcPath, n, size)
	}

	// fsync + close is the floor of the cache-defeating verify contract
	// (see verify.go's doc comment) -- do it here, immediately after the
	// write, rather than leaving it to whatever calls Verify later.
	if syncErr := archiveFile.Sync(); syncErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: fsync archive dest %s: %w", archivePath, syncErr)
	}
	if syncErr := localFile.Sync(); syncErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: fsync local dest %s: %w", localPath, syncErr)
	}
	if closeErr := archiveFile.Close(); closeErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: close archive dest %s: %w", archivePath, closeErr)
	}
	if closeErr := localFile.Close(); closeErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: close local dest %s: %w", localPath, closeErr)
	}

	// Fsync both parent directories to ensure the directory entries are
	// durable on power loss. A crash between the file's own fsync and the
	// OS flushing the parent directory's metadata can silently lose the
	// file. Errors are logged, not fatal -- the file is on disk; only the
	// directory entry might not be durable, which is the same class of
	// risk as os.Chtimes failures (#103). Dedupe when both destinations
	// share a parent directory.
	archiveDir := filepath.Dir(archivePath)
	localDir := filepath.Dir(localPath)
	dirs := []string{archiveDir}
	if localDir != archiveDir {
		dirs = append(dirs, localDir)
	}
	for _, dir := range dirs {
		if dirErr := syncParentDirFn(dir); dirErr != nil {
			slog.Warn("dualwrite: dir fsync failed; entry may not be durable on power loss", "dir", dir, "err", dirErr)
		}
	}

	return WriteResult{
		ArchivePath: archivePath,
		LocalPath:   localPath,
		SizeBytes:   size,
		FullHash:    fmt.Sprintf("%x", fullHasher.Sum(nil)),
		FastHash:    fastHasher.Sum(),
	}, nil
}

// createExclusive makes path's parent directories (if missing) and opens
// path for writing, refusing to overwrite an existing file (O_EXCL).
func createExclusive(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // destination under an operator-configured archive/edit root
	if err != nil {
		return nil, err
	}
	return f, nil
}
