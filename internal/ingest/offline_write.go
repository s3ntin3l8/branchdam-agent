package ingest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// ErrArchiveConflict is returned (wrapped, with the paths and an
// operator-facing remedy) by CopyToArchive when a file it did not write
// already sits at the final archive path and does not match the queued
// hash. CopyToArchive never deletes or overwrites such a file -- it may be
// another card's verified Tier-3 master that happens to share a name --
// so retrying cannot help until someone moves one of the two files; Drain
// marks the row's archive copy FAILED instead of retrying forever.
var ErrArchiveConflict = errors.New("archive destination exists with different content")

// ErrLocalCopyMismatch is returned (wrapped) by CopyToArchive when the
// local edit copy no longer hashes to the BLAKE3 recorded at ingest time
// (typically: it was edited in place before the archive copy ran). The
// mismatching bytes are never placed at the archive path; like
// ErrArchiveConflict this cannot heal by retrying, so Drain fails the row.
var ErrLocalCopyMismatch = errors.New("local copy no longer matches the queued hash")

// archiveVerifyFn is the Verify CopyToArchive runs against the archive
// path (both the "already there?" check and the post-rename check). A
// package var so tests can inject a read failure; production is Verify.
var archiveVerifyFn = Verify

// errDestinationExists is renameNoClobber's refusal.
var errDestinationExists = errors.New("destination already exists")

// agentTempSuffix marks every in-flight file this package writes next to a
// final destination. A file with this name is, by construction, the
// agent's own incomplete or not-yet-committed copy -- the only kind of
// file the offline path ever deletes on its own initiative.
const agentTempSuffix = ".branchdam-agent-tmp"

// agentTempName is deterministic (not os.CreateTemp's random suffix) so a
// leftover from a killed attempt is recognizable and reusable across a
// process restart, rather than accumulating an ever-growing set of
// abandoned temp files with unrelated random names. Used for both the
// offline local edit copy (ingestFileOffline) and the archive copy
// (CopyToArchive).
func agentTempName(finalPath string) string {
	dir, base := filepath.Split(finalPath)
	return filepath.Join(dir, "."+base+agentTempSuffix)
}

// tempArchiveName is agentTempName for an archive destination.
func tempArchiveName(archivePath string) string {
	return agentTempName(archivePath)
}

// renameNoClobber moves tmpPath (a complete, fsynced file of the agent's
// own) to finalPath, refusing with errDestinationExists if anything is
// already at finalPath. os.Rename alone would silently replace it on every
// platform this agent ships for. The Lstat-then-rename pair leaves a
// window only against another process creating finalPath in between --
// this agent never does, and every final name it picks was free a moment
// earlier.
func renameNoClobber(tmpPath, finalPath string) error {
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("ingest: place %s: %w", finalPath, errDestinationExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ingest: stat %s before rename: %w", finalPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("ingest: rename %s to %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

// CopyToArchive copies localPath (already verified against wantFullHash at
// ingest time) into archivePath and cache-defeating-verifies the result,
// once connectivity/mount to the archive destination is available. It is
// crash-safe and idempotent across repeated calls for the same archivePath,
// and it never deletes or overwrites a file it did not write:
//
//   - If archivePath already exists and verifies against wantFullHash, the
//     copy already completed on an earlier call (the process was killed
//     after the copy but before the caller recorded that fact), or an
//     identical file was already there -- return success without copying.
//   - If archivePath exists but does NOT verify, it is not ours: this
//     function only ever renames a complete, hash-checked temp file into
//     place, so a non-matching file at the final name is someone else's --
//     possibly another card's verified master with the same rendered name.
//     Leave it alone and return ErrArchiveConflict.
//   - The copy itself never writes directly to archivePath: it writes to a
//     deterministically-named temp file in the same directory (so the
//     rename is same-filesystem-atomic), hashes the bytes as they stream,
//     and renames into place (refusing to clobber) only after a successful
//     fsync+close and a hash match. A local copy that no longer matches
//     wantFullHash returns ErrLocalCopyMismatch and never reaches the final
//     name. Any leftover temp file from a previous killed attempt is
//     removed before starting -- it never got renamed into place and is
//     therefore incomplete, and the name marks it as the agent's own.
//
// opts applies only to the copy itself, never to either of the two
// internal Verify calls above -- those are this function's own idempotency
// safety net (see the doc comment above), a distinct phase from "copying"
// that a caller-supplied byte-count callback can't be relabeled as after
// the fact. A caller wanting verify-phase progress for the archive
// destination should call Verify directly.
func CopyToArchive(localPath, archivePath, wantFullHash string, opts ...WriteOption) error {
	if existing, err := os.Lstat(archivePath); err == nil {
		// Only a regular file can be "already done". Anything else at the
		// final name -- a directory, a device, or a symlink, even one that
		// points at an identical file -- is a conflict: following symlinks
		// at the archive path is a deliberate future opt-in, not a default.
		if existing.Mode().IsRegular() {
			vr, verr := archiveVerifyFn(archivePath, wantFullHash)
			if verr != nil {
				// Couldn't read it (flaky mount?): neither "done" nor a
				// proven conflict -- let the caller retry later.
				return fmt.Errorf("ingest: verify existing archive dest %s: %w", archivePath, verr)
			}
			if vr.Verified {
				return nil
			}
		}
		return fmt.Errorf("%w: %s already exists and does not match the queued hash (BLAKE3 %s); nothing was overwritten -- move or rename the existing file, then retry this queue row",
			ErrArchiveConflict, archivePath, wantFullHash)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ingest: stat archive dest %s: %w", archivePath, err)
	}

	tmpPath := tempArchiveName(archivePath)
	_ = os.Remove(tmpPath) // our own leftover from a previous killed attempt, if any -- never renamed, so never complete

	src, err := os.Open(localPath) //nolint:gosec // localPath is our own previously-written, verified copy
	if err != nil {
		return fmt.Errorf("ingest: open local copy %s: %w", localPath, err)
	}
	defer func() { _ = src.Close() }()

	dst, err := createExclusive(tmpPath)
	if err != nil {
		return fmt.Errorf("ingest: create archive temp dest %s: %w", tmpPath, err)
	}

	hasher := blake3.New()
	writers := []io.Writer{dst, hasher}
	if o := applyWriteOptions(opts); o.onBytes != nil {
		writers = append(writers, &progressWriter{onBytes: o.onBytes})
	}
	if _, err := io.Copy(io.MultiWriter(writers...), src); err != nil {
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
	if got := fmt.Sprintf("%x", hasher.Sum(nil)); got != wantFullHash {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("%w: %s hashes to %s but was queued as %s -- was it edited before the archive copy? Nothing was written to %s",
			ErrLocalCopyMismatch, localPath, got, wantFullHash, archivePath)
	}

	if err := renameNoClobber(tmpPath, archivePath); err != nil {
		_ = os.Remove(tmpPath)
		return err // transient: the next attempt re-examines whatever is there now
	}

	vr, err := archiveVerifyFn(archivePath, wantFullHash)
	if err != nil {
		// Couldn't re-read it (flaky mount, transport error): that says
		// nothing about the bytes, which were hash-checked on the way in.
		// Leave the placed file alone and report a transient error -- the
		// next attempt's existing-file check re-verifies it and either
		// finds it done or reports a conflict, never deletes it.
		return fmt.Errorf("ingest: verify archive copy %s (left in place, will re-check): %w", archivePath, err)
	}
	if !vr.Verified {
		// The file at archivePath is the one this call just renamed into
		// place (renameNoClobber refused any other), so removing it
		// deletes only the agent's own bad copy.
		_ = os.Remove(archivePath)
		return fmt.Errorf("ingest: archive copy %s did not verify (got %s, want %s) -- removed, will retry", archivePath, vr.Hash, wantFullHash)
	}
	return nil
}
