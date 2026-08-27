package ingest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

// WriteLocal reads srcPath exactly once and writes it to localPath only --
// the offline-ingest counterpart to DualWrite, used when the archive
// destination isn't reachable (issue #4's "write the local edit copy
// immediately, unconditionally" step). Same O_EXCL/fsync/close discipline as
// DualWrite, so the two are interchangeable inputs to Verify.
func WriteLocal(srcPath, localPath string, opts ...WriteOption) (result WriteResult, err error) {
	var created string
	defer func() {
		if err != nil && created != "" {
			_ = os.Remove(created)
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

	localFile, err := createExclusive(localPath)
	if err != nil {
		return WriteResult{}, fmt.Errorf("ingest: create local dest %s: %w", localPath, err)
	}
	created = localPath
	defer func() { _ = localFile.Close() }()

	fullHasher := blake3.New()
	fastHasher := hashing.NewStreamingFastHasher(size)

	writers := []io.Writer{localFile, fullHasher, fastHasher}
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

	if syncErr := localFile.Sync(); syncErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: fsync local dest %s: %w", localPath, syncErr)
	}
	if closeErr := localFile.Close(); closeErr != nil {
		return WriteResult{}, fmt.Errorf("ingest: close local dest %s: %w", localPath, closeErr)
	}

	return WriteResult{
		LocalPath: localPath,
		SizeBytes: size,
		FullHash:  fmt.Sprintf("%x", fullHasher.Sum(nil)),
		FastHash:  fastHasher.Sum(),
	}, nil
}

// tempArchiveName is deterministic (not os.CreateTemp's random suffix) so a
// leftover from a killed CopyToArchive attempt is recognizable and reusable
// across a process restart, rather than accumulating an ever-growing set of
// abandoned temp files with unrelated random names.
func tempArchiveName(archivePath string) string {
	dir, base := filepath.Split(archivePath)
	return filepath.Join(dir, "."+base+".branchdam-agent-tmp")
}

// CopyToArchive copies localPath (already verified against wantFullHash at
// ingest time) into archivePath and cache-defeating-verifies the result,
// once connectivity/mount to the archive destination is available. It is
// crash-safe and idempotent across repeated calls for the same archivePath:
//
//   - If archivePath already exists and verifies against wantFullHash, the
//     copy already completed on an earlier call (the process was killed
//     after the copy but before the caller recorded that fact) -- return
//     success without copying again.
//   - If archivePath exists but does NOT verify, it's neither a clean
//     "already done" state nor safe to trust -- remove it and redo the copy.
//   - The copy itself never writes directly to archivePath: it writes to a
//     deterministically-named temp file in the same directory (so the
//     rename is same-filesystem-atomic) and renames into place only after a
//     successful fsync+close. This is what DualWrite's own doc comment
//     requires of any caller that retries after a crash -- see writer.go --
//     and it's the difference between a stuck retry loop (O_EXCL forever
//     colliding with a partial file under the final name) and a clean
//     resume: any leftover temp file from a previous killed attempt is
//     unconditionally removed before starting, since by definition it never
//     got renamed into place and is therefore incomplete.
func CopyToArchive(localPath, archivePath, wantFullHash string, opts ...WriteOption) error {
	if existing, err := os.Stat(archivePath); err == nil && !existing.IsDir() {
		vr, verr := Verify(archivePath, wantFullHash, opts...)
		if verr == nil && vr.Verified {
			return nil
		}
		if err := os.Remove(archivePath); err != nil {
			return fmt.Errorf("ingest: remove unverified existing archive dest %s: %w", archivePath, err)
		}
	}

	tmpPath := tempArchiveName(archivePath)
	_ = os.Remove(tmpPath) // leftover from a previous killed attempt, if any -- never renamed, so never complete

	src, err := os.Open(localPath) //nolint:gosec // localPath is our own previously-written, verified copy
	if err != nil {
		return fmt.Errorf("ingest: open local copy %s: %w", localPath, err)
	}
	defer func() { _ = src.Close() }()

	dst, err := createExclusive(tmpPath)
	if err != nil {
		return fmt.Errorf("ingest: create archive temp dest %s: %w", tmpPath, err)
	}

	var dstW io.Writer = dst
	if o := applyWriteOptions(opts); o.onBytes != nil {
		dstW = io.MultiWriter(dst, &progressWriter{onBytes: o.onBytes})
	}
	if _, err := io.Copy(dstW, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: copy %s to archive: %w", localPath, err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: fsync archive temp dest %s: %w", tmpPath, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: close archive temp dest %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, archivePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("ingest: rename %s to %s: %w", tmpPath, archivePath, err)
	}

	vr, err := Verify(archivePath, wantFullHash, opts...)
	if err != nil {
		return fmt.Errorf("ingest: verify archive copy %s: %w", archivePath, err)
	}
	if !vr.Verified {
		_ = os.Remove(archivePath)
		return fmt.Errorf("ingest: archive copy %s did not verify (got %s, want %s) -- removed, will retry", archivePath, vr.Hash, wantFullHash)
	}
	return nil
}
