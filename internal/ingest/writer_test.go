package ingest

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

func writeSourceFile(t *testing.T, dir string, size int) string {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.New(rand.NewSource(42)).Read(data) //nolint:gosec // deterministic test fixture, not security-sensitive
	path := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func TestDualWriteProducesTwoIdenticalVerifiedCopies(t *testing.T) {
	sizes := []int{0, 1024, 3 * 1024 * 1024}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			src := writeSourceFile(t, dir, size)
			srcData, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}

			archivePath := filepath.Join(dir, "archive", "2026", "photo.bin")
			localPath := filepath.Join(dir, "local", "2026", "photo.bin")

			res, err := DualWrite(src, archivePath, localPath)
			if err != nil {
				t.Fatalf("DualWrite: %v", err)
			}
			if res.SizeBytes != int64(size) {
				t.Errorf("SizeBytes = %d, want %d", res.SizeBytes, size)
			}

			wantFull := blake3.New()
			_, _ = wantFull.Write(srcData)
			wantFullHash := hex.EncodeToString(wantFull.Sum(nil))
			if res.FullHash != wantFullHash {
				t.Errorf("FullHash = %s, want %s", res.FullHash, wantFullHash)
			}

			wantFast, err := hashing.FastHash(bytes.NewReader(srcData), int64(size))
			if err != nil {
				t.Fatal(err)
			}
			if res.FastHash != wantFast {
				t.Errorf("FastHash = %s, want %s", res.FastHash, wantFast)
			}

			for _, dest := range []string{archivePath, localPath} {
				got, err := os.ReadFile(dest)
				if err != nil {
					t.Fatalf("read %s: %v", dest, err)
				}
				if !bytes.Equal(got, srcData) {
					t.Errorf("%s content mismatch", dest)
				}
			}

			// Verify must succeed against both copies and report the hash
			// actually computed from re-reading the file, matching what
			// DualWrite reported.
			for _, dest := range []string{archivePath, localPath} {
				vr, err := Verify(dest, res.FullHash)
				if err != nil {
					t.Fatalf("Verify(%s): %v", dest, err)
				}
				if !vr.Verified {
					t.Errorf("Verify(%s).Verified = false, hash %s vs want %s (method %s)", dest, vr.Hash, res.FullHash, vr.Method)
				}
				t.Logf("Verify(%s) used method=%s", dest, vr.Method)
			}
		})
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 4096)
	archivePath := filepath.Join(dir, "archive.bin")
	localPath := filepath.Join(dir, "local.bin")

	res, err := DualWrite(src, archivePath, localPath)
	if err != nil {
		t.Fatalf("DualWrite: %v", err)
	}

	// Corrupt the archive copy after the fact and confirm Verify catches it.
	if err := os.WriteFile(archivePath, []byte("corrupted-not-the-original-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	vr, err := Verify(archivePath, res.FullHash)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if vr.Verified {
		t.Error("Verify reported success against corrupted content")
	}
}

func TestDualWriteRefusesToOverwriteExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 100)
	archivePath := filepath.Join(dir, "archive.bin")
	localPath := filepath.Join(dir, "local.bin")

	if _, err := DualWrite(src, archivePath, localPath); err != nil {
		t.Fatalf("first DualWrite: %v", err)
	}
	if _, err := DualWrite(src, archivePath, localPath); err == nil {
		t.Error("second DualWrite over existing destinations succeeded, want an error (O_EXCL)")
	}
}

func TestDualWriteSourceDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := DualWrite(filepath.Join(dir, "does-not-exist.bin"), filepath.Join(dir, "archive.bin"), filepath.Join(dir, "local.bin"))
	if err == nil {
		t.Fatal("expected an error for a nonexistent source file")
	}
}

// TestCreateExclusiveMkdirFails covers createExclusive's MkdirAll error
// branch: a path component that already exists as a regular file (not a
// directory) can never be created as a directory underneath it.
func TestCreateExclusiveMkdirFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// blocker is a regular file; treating it as a directory component must fail.
	_, err := createExclusive(filepath.Join(blocker, "subdir", "dest.bin"))
	if err == nil {
		t.Fatal("expected createExclusive to fail when a path component is a regular file")
	}
}

// TestDualWriteCleansUpArchiveCopyWhenLocalCreateFails proves that when the
// SECOND destination's O_EXCL create fails (a pre-existing file at
// localPath, or any other setup error), DualWrite does not leave a
// zero-byte (or partial) file behind at the FIRST destination -- a stray
// leftover there would silently block every future retry with a confusing
// "file exists" error despite nothing having actually been archived, and
// worse, could be mistaken for a real complete master by a human or a
// concurrent scan of the same directory.
func TestDualWriteCleansUpArchiveCopyWhenLocalCreateFails(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 4096)
	archivePath := filepath.Join(dir, "archive.bin")
	localPath := filepath.Join(dir, "local.bin")

	// Pre-create localPath so DualWrite's O_EXCL create of the SECOND
	// destination fails after the FIRST has already succeeded.
	if err := os.WriteFile(localPath, []byte("pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := DualWrite(src, archivePath, localPath); err == nil {
		t.Fatal("expected DualWrite to fail when the local destination already exists")
	}

	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Errorf("archive destination should have been cleaned up after local create failed, stat err = %v", err)
	}
}

// TestDualWriteFsyncsParentDirs proves that DualWrite calls syncParentDirFn
// for both the archive and local destination parent directories. The test
// substitutes a recording stub for syncParentDirFn and asserts it was
// called exactly once with each expected directory path, in any order.
func TestDualWriteFsyncsParentDirs(t *testing.T) {
	orig := syncParentDirFn
	t.Cleanup(func() { syncParentDirFn = orig })

	var dirs []string
	syncParentDirFn = func(dir string) error {
		dirs = append(dirs, dir)
		return nil
	}

	dir := t.TempDir()
	src := writeSourceFile(t, dir, 4096)
	archiveDir := filepath.Join(dir, "archive")
	localDir := filepath.Join(dir, "local")
	archivePath := filepath.Join(archiveDir, "photo.bin")
	localPath := filepath.Join(localDir, "photo.bin")

	if _, err := DualWrite(src, archivePath, localPath); err != nil {
		t.Fatalf("DualWrite: %v", err)
	}

	if len(dirs) != 2 {
		t.Fatalf("syncParentDirFn called %d times, want 2", len(dirs))
	}

	// Both dirs must appear exactly once, in any order.
	for _, want := range []string{archiveDir, localDir} {
		count := 0
		for _, got := range dirs {
			if got == want {
				count++
			}
		}
		if count != 1 {
			t.Errorf("syncParentDirFn called with %s %d times, want 1", want, count)
		}
	}
}

// TestSyncParentDirFnErrorLoggedNotFatal proves that a syncParentDirFn error
// does not fail DualWrite -- the file is on disk; only the directory entry
// might not be durable, which is the same class of risk as os.Chtimes
// failures (#103). The slog.Warn path is implicitly exercised here (no
// suppress, just verify the ingest completes).
func TestSyncParentDirFnErrorLoggedNotFatal(t *testing.T) {
	orig := syncParentDirFn
	t.Cleanup(func() { syncParentDirFn = orig })

	syncParentDirFn = func(dir string) error {
		return fmt.Errorf("simulated dir fsync failure")
	}

	dir := t.TempDir()
	src := writeSourceFile(t, dir, 4096)
	archivePath := filepath.Join(dir, "archive.bin")
	localPath := filepath.Join(dir, "local.bin")

	if _, err := DualWrite(src, archivePath, localPath); err != nil {
		t.Fatalf("DualWrite should succeed even when syncParentDirFn fails; got: %v", err)
	}
}

// TestDualWriteFsyncsSharedParentOnce proves that when archive and local
// share the same parent directory, syncParentDirFn is called only once
// for that directory (not redundantly twice).
func TestDualWriteFsyncsSharedParentOnce(t *testing.T) {
	orig := syncParentDirFn
	t.Cleanup(func() { syncParentDirFn = orig })

	var dirs []string
	syncParentDirFn = func(dir string) error {
		dirs = append(dirs, dir)
		return nil
	}

	dir := t.TempDir()
	src := writeSourceFile(t, dir, 4096)
	sharedDir := filepath.Join(dir, "shared")
	archivePath := filepath.Join(sharedDir, "photo.bin")
	localPath := filepath.Join(sharedDir, "copy.bin")

	if _, err := DualWrite(src, archivePath, localPath); err != nil {
		t.Fatalf("DualWrite: %v", err)
	}

	if len(dirs) != 1 {
		t.Fatalf("syncParentDirFn called %d times, want 1 (shared parent deduped)", len(dirs))
	}
	if dirs[0] != sharedDir {
		t.Errorf("syncParentDirFn called with %s, want %s", dirs[0], sharedDir)
	}
}

// TestSyncParentDirReal exercises the real syncParentDir implementation on a
// temp directory, catching platform-specific fsync regressions that the
// stub-based tests would miss. Only the positive path is tested (clean
// dir → no error); the error path is covered by TestSyncParentDirFnErrorLoggedNotFatal.
func TestSyncParentDirReal(t *testing.T) {
	dir := t.TempDir()
	if err := syncParentDir(dir); err != nil {
		t.Errorf("syncParentDir(%s) failed on real directory: %v", dir, err)
	}
}
