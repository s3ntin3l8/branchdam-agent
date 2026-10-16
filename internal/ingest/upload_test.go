package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// recordingUploader is a fake /upload endpoint: it reads the body,
// records what it was sent, and answers with a relativePath chosen by
// relPath (default "2026/<filename>").
type recordingUploader struct {
	fakeClient
	calls      []branchdam.UploadOptions
	bodies     [][]byte
	beforeRead func(opts branchdam.UploadOptions)
	relPath    func(opts branchdam.UploadOptions) string
	status     string // default "UPLOADED"
	err        error
}

func (u *recordingUploader) Upload(_ context.Context, r io.Reader, opts branchdam.UploadOptions) (*branchdam.UploadResponse, error) {
	if u.beforeRead != nil {
		u.beforeRead(opts)
	}
	u.calls = append(u.calls, opts)
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	u.bodies = append(u.bodies, body)
	if u.err != nil {
		return nil, u.err
	}
	rel := "2026/" + opts.Filename
	if u.relPath != nil {
		rel = u.relPath(opts)
	}
	status := "UPLOADED"
	if u.status != "" {
		status = u.status
	}
	return &branchdam.UploadResponse{
		NodeUUID:     fmt.Sprintf("node-%d", len(u.calls)),
		Status:       status,
		BytesWritten: int64(len(body)),
		Blake3Hash:   blake3Bytes(body),
		RelativePath: rel,
	}, nil
}

func blake3Bytes(b []byte) string {
	h := blake3.New()
	_, _ = h.Write(b)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func newUploadEngine(u uploader, localRoot string) *Engine {
	e := NewEngine(nil, "test-agent", config.IngestConfig{
		LocalEditRoot: localRoot,
		UploadStream:  true,
		PathTemplate:  "2026/{original_name}",
	}, nil)
	e.Uploader = u
	e.Exiftool = &Exiftool{}
	e.IsMetered = func() (bool, error) { return false, nil }
	return e
}

func writeUploadCardFile(t *testing.T, cardRoot, name, content string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(cardRoot, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// noAgentTemps fails if any .branchdam-agent-tmp file is left under root.
func noAgentTemps(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, agentTempSuffix) {
			t.Errorf("leftover agent temp file %s", p)
		}
		return nil
	})
}

// TestUploadStreamReadsCardOnceAndSendsHash pins the A8 core: the upload
// body is the verified local copy, not a second read of the card, and it
// carries the BLAKE3 of what left the card. The fake server rewrites the
// card file before it reads the body -- a body streamed from the card
// would then carry the new bytes.
func TestUploadStreamReadsCardOnceAndSendsHash(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	const original = "original card bytes"
	src := writeUploadCardFile(t, cardRoot, "IMG_0001.JPG", original, time.Time{})

	u := &recordingUploader{beforeRead: func(branchdam.UploadOptions) {
		if err := os.WriteFile(src, []byte("CARD CHANGED AFTER COPY"), 0o644); err != nil {
			t.Fatal(err)
		}
	}}
	res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("file error: %v", fr.Err)
	}
	if len(u.calls) != 1 {
		t.Fatalf("upload calls = %d, want 1", len(u.calls))
	}
	if got := string(u.bodies[0]); got != original {
		t.Errorf("uploaded body = %q, want the bytes copied off the card (%q)", got, original)
	}
	want := blake3Bytes([]byte(original))
	if u.calls[0].Blake3Hash != want {
		t.Errorf("X-Blake3-Hash = %q, want %q", u.calls[0].Blake3Hash, want)
	}
	if u.calls[0].SizeBytes != int64(len(original)) {
		t.Errorf("SizeBytes = %d, want %d", u.calls[0].SizeBytes, len(original))
	}
	local := filepath.Join(localRoot, "2026", "IMG_0001.JPG")
	if fr.LocalPath != local {
		t.Errorf("LocalPath = %q, want %q", fr.LocalPath, local)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != original {
		t.Errorf("local copy = %q, %v; want %q", b, err, original)
	}
	if !fr.LocalVerify.Verified {
		t.Error("local copy not verified")
	}
	noAgentTemps(t, localRoot)
}

func TestSafeRelativePath(t *testing.T) {
	ok := map[string]string{
		"2026/IMG_0001.JPG":       filepath.Join("2026", "IMG_0001.JPG"),
		"a/./b.jpg":               filepath.Join("a", "b.jpg"),
		"IMG.JPG":                 "IMG.JPG",
		"2026/..foo/IMG_0001.JPG": filepath.Join("2026", "..foo", "IMG_0001.JPG"),
	}
	for in, want := range ok {
		got, err := safeRelativePath(in)
		if err != nil || got != want {
			t.Errorf("safeRelativePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", ".", "/etc/passwd", "../evil.jpg", "2026/../../evil.jpg", "a/../b.jpg",
		"C:/Windows/evil.jpg", "c:evil.jpg", `..\evil.jpg`, `2026\IMG.JPG`, "a/\x00b",
	} {
		if got, err := safeRelativePath(in); !errors.Is(err, errUnsafeRelativePath) {
			t.Errorf("safeRelativePath(%q) = %q, %v; want errUnsafeRelativePath", in, got, err)
		}
	}
}

// TestUploadStreamRefusesEscapingRelativePath: a server relativePath that
// climbs out of LocalEditRoot fails the file and writes nothing outside.
func TestUploadStreamRefusesEscapingRelativePath(t *testing.T) {
	for _, rel := range []string{"../../escaped.jpg", "/abs/escaped.jpg"} {
		t.Run(rel, func(t *testing.T) {
			dir := t.TempDir()
			cardRoot := filepath.Join(dir, "card")
			localRoot := filepath.Join(dir, "a", "local")
			writeUploadCardFile(t, cardRoot, "IMG_0001.JPG", "bytes", time.Time{})

			u := &recordingUploader{relPath: func(branchdam.UploadOptions) string { return rel }}
			res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
			if err != nil {
				t.Fatalf("IngestCard: %v", err)
			}
			if fr := res.Files[0]; !errors.Is(fr.Err, errUnsafeRelativePath) {
				t.Fatalf("Err = %v, want errUnsafeRelativePath", fr.Err)
			}
			if _, err := os.Stat(filepath.Join(dir, "escaped.jpg")); !os.IsNotExist(err) {
				t.Errorf("file written outside LocalEditRoot (stat err = %v)", err)
			}
			if _, err := os.Stat("/abs/escaped.jpg"); !os.IsNotExist(err) {
				t.Errorf("file written at an absolute path (stat err = %v)", err)
			}
			noAgentTemps(t, localRoot)
		})
	}
}

// TestUploadStreamMeteredRetryReusesLocalCopy pins the O_EXCL retry
// failure: re-inserting a card while still metered used to fail on the
// local copy the first pass left; the unmetered pass then uploads and
// keeps that same copy.
func TestUploadStreamMeteredRetryReusesLocalCopy(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	writeUploadCardFile(t, cardRoot, "IMG_0001.JPG", "metered bytes", time.Time{})
	local := filepath.Join(localRoot, "2026", "IMG_0001.JPG")

	u := &recordingUploader{}
	e := newUploadEngine(u, localRoot)
	e.Ingest.PauseUploadOnMetered = true
	e.IsMetered = func() (bool, error) { return true, nil }

	for pass := 1; pass <= 2; pass++ {
		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("pass %d: IngestCard: %v", pass, err)
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("pass %d: file error: %v", pass, fr.Err)
		}
		if !fr.Deferred || !fr.Skipped {
			t.Errorf("pass %d: Deferred=%v Skipped=%v, want both", pass, fr.Deferred, fr.Skipped)
		}
		if fr.LocalPath != local {
			t.Errorf("pass %d: LocalPath = %q, want %q", pass, fr.LocalPath, local)
		}
	}
	if len(u.calls) != 0 {
		t.Fatalf("upload calls while metered = %d, want 0", len(u.calls))
	}

	e.IsMetered = func() (bool, error) { return false, nil }
	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("unmetered: IngestCard: %v", err)
	}
	fr := res.Files[0]
	if fr.Err != nil || fr.Deferred {
		t.Fatalf("unmetered: Err=%v Deferred=%v", fr.Err, fr.Deferred)
	}
	if len(u.calls) != 1 || fr.LocalPath != local {
		t.Fatalf("unmetered: calls=%d LocalPath=%q, want 1 and %q", len(u.calls), fr.LocalPath, local)
	}
	entries, _ := os.ReadDir(filepath.Dir(local))
	if len(entries) != 1 {
		t.Errorf("local dir holds %d entries, want just the one copy", len(entries))
	}
	noAgentTemps(t, localRoot)
}

// TestUploadStreamSidecarFollowsMaster: a sidecar is uploaded after its
// master with the master's capture time and final (server-suffixed) stem,
// so the server renders it into the master's directory under the
// master's name -- even when the walk reaches the sidecar first.
func TestUploadStreamSidecarFollowsMaster(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	masterTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	sidecarTime := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
	// "IMG_0001.XMP" sorts before "IMG_0001.dng" ('X' < 'd').
	writeUploadCardFile(t, cardRoot, "DCIM/IMG_0001.XMP", "<xmp/>", sidecarTime)
	writeUploadCardFile(t, cardRoot, "DCIM/IMG_0001.dng", "raw bytes", masterTime)
	writeUploadCardFile(t, cardRoot, "DCIM/ORPHAN.srt", "1\n", sidecarTime)

	u := &recordingUploader{relPath: func(o branchdam.UploadOptions) string {
		// The server already has an IMG_0001.dng: it suffixes the new one.
		if o.Filename == "IMG_0001.dng" {
			return "2026/IMG_0001_1.dng"
		}
		return "2026/" + o.Filename
	}}
	res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Fatalf("%s: %v", fr.SourcePath, fr.Err)
		}
	}
	if len(u.calls) != 3 {
		t.Fatalf("upload calls = %d, want 3", len(u.calls))
	}
	if u.calls[0].Filename != "IMG_0001.dng" {
		t.Fatalf("first upload = %q, want the master", u.calls[0].Filename)
	}
	byName := map[string]branchdam.UploadOptions{}
	for _, c := range u.calls[1:] {
		byName[c.Filename] = c
	}
	xmp, ok := byName["IMG_0001_1.XMP"]
	if !ok {
		t.Fatalf("sidecar not renamed after the master's final stem; uploads: %+v", u.calls)
	}
	if xmp.CaptureTimestamp != masterTime.Unix() {
		t.Errorf("sidecar X-Capture-Timestamp = %d, want the master's %d", xmp.CaptureTimestamp, masterTime.Unix())
	}
	orphan, ok := byName["ORPHAN.srt"]
	if !ok || orphan.CaptureTimestamp != sidecarTime.Unix() {
		t.Errorf("orphan sidecar = %+v (found=%v), want its own name and mtime", orphan, ok)
	}
	if _, err := os.Stat(filepath.Join(localRoot, "2026", "IMG_0001_1.XMP")); err != nil {
		t.Errorf("sidecar local copy not next to the master's: %v", err)
	}
}

// TestUploadStreamSidecarHeldWhenMasterFails: a sidecar whose master
// failed is not uploaded on its own (it would land detached).
func TestUploadStreamSidecarHeldWhenMasterFails(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeUploadCardFile(t, cardRoot, "DJI_0001.MP4", "video", time.Time{})
	writeUploadCardFile(t, cardRoot, "DJI_0001.SRT", "1\n", time.Time{})

	u := &recordingUploader{err: errors.New("server down")}
	res, err := newUploadEngine(u, filepath.Join(dir, "local")).IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(res.Files))
	}
	if len(u.calls) != 1 {
		t.Errorf("upload calls = %d, want only the master's", len(u.calls))
	}
	if sc := res.Files[1]; sc.Err == nil || !strings.Contains(sc.Err.Error(), "held back") {
		t.Errorf("sidecar Err = %v, want held back", sc.Err)
	}
	noAgentTemps(t, filepath.Join(dir, "local"))
}

// TestUploadStreamSidecarDeferredWithMaster: on a metered link the
// sidecar's local copy goes next to the master's deferred copy.
func TestUploadStreamSidecarDeferredWithMaster(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	writeUploadCardFile(t, cardRoot, "DJI_0001.MP4", "video", time.Time{})
	writeUploadCardFile(t, cardRoot, "DJI_0001.SRT", "1\n", time.Time{})

	u := &recordingUploader{}
	e := newUploadEngine(u, localRoot)
	e.Ingest.PauseUploadOnMetered = true
	e.IsMetered = func() (bool, error) { return true, nil }
	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	for _, fr := range res.Files {
		if fr.Err != nil || !fr.Deferred {
			t.Errorf("%s: Err=%v Deferred=%v", fr.SourcePath, fr.Err, fr.Deferred)
		}
	}
	if got, want := res.Files[1].LocalPath, filepath.Join(localRoot, "2026", "DJI_0001.SRT"); got != want {
		t.Errorf("sidecar LocalPath = %q, want %q", got, want)
	}
	if len(u.calls) != 0 {
		t.Errorf("upload calls = %d, want 0", len(u.calls))
	}

	// Re-inserting the card while still metered reuses both local copies:
	// neither the master nor its sidecar is copied off the card again.
	res, err = e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("second IngestCard: %v", err)
	}
	for _, fr := range res.Files {
		if fr.Err != nil || !fr.Deferred || fr.Write.SizeBytes != 0 {
			t.Errorf("%s on re-insert: Err=%v Deferred=%v copied=%d bytes; want deferred with no copy",
				fr.SourcePath, fr.Err, fr.Deferred, fr.Write.SizeBytes)
		}
	}
	noAgentTemps(t, localRoot)
}

// TestUploadStreamFailureLeavesNoLocalFiles: a failed upload removes the
// staged copy -- no temp and nothing at a final name.
func TestUploadStreamFailureLeavesNoLocalFiles(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	writeUploadCardFile(t, cardRoot, "IMG_0001.JPG", "bytes", time.Time{})

	u := &recordingUploader{err: &branchdam.HTTPError{StatusCode: 503}}
	res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if res.Files[0].Err == nil {
		t.Fatal("want an error")
	}
	_ = filepath.WalkDir(localRoot, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("left %s behind", p)
		}
		return nil
	})
}

// TestUploadStreamLocalConflictNeverOverwrites: a different file at the
// server's relativePath under LocalEditRoot is left alone.
func TestUploadStreamLocalConflictNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	localRoot := filepath.Join(dir, "local")
	writeUploadCardFile(t, cardRoot, "IMG_0001.JPG", "new card bytes", time.Time{})
	existing := writeUploadCardFile(t, localRoot, "2026/IMG_0001.JPG", "someone else's edit", time.Time{})

	u := &recordingUploader{}
	res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if fr := res.Files[0]; !errors.Is(fr.Err, errLocalCopyConflict) {
		t.Fatalf("Err = %v, want errLocalCopyConflict", fr.Err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "someone else's edit" {
		t.Errorf("existing local file changed to %q", b)
	}
	noAgentTemps(t, localRoot)
}

// TestUploadStreamDeduplicatedPlacesAtExistingPath: the server already
// holds the content and answers DEDUPLICATED with the existing node's
// relativePath. The verified local copy lands at that path (not the
// template's), and when an identical file is already there it is kept as
// is -- the upload is a success either way, and nothing is overwritten.
func TestUploadStreamDeduplicatedPlacesAtExistingPath(t *testing.T) {
	const content = "already archived bytes"
	for _, tc := range []struct {
		name        string
		preexisting bool
	}{
		{"placed at the existing path", false},
		{"identical file already there is kept", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cardRoot := filepath.Join(dir, "card")
			localRoot := filepath.Join(dir, "local")
			writeUploadCardFile(t, cardRoot, "IMG_0002.JPG", content, time.Time{})
			want := filepath.Join(localRoot, "2025", "EARLIER.JPG")
			if tc.preexisting {
				writeUploadCardFile(t, localRoot, "2025/EARLIER.JPG", content, time.Time{})
			}

			u := &recordingUploader{
				status:  "DEDUPLICATED",
				relPath: func(branchdam.UploadOptions) string { return "2025/EARLIER.JPG" },
			}
			res, err := newUploadEngine(u, localRoot).IngestCard(context.Background(), cardRoot)
			if err != nil {
				t.Fatalf("IngestCard: %v", err)
			}
			fr := res.Files[0]
			if fr.Err != nil {
				t.Fatalf("file error: %v", fr.Err)
			}
			if fr.LocalPath != want {
				t.Errorf("LocalPath = %q, want the existing node's path %q", fr.LocalPath, want)
			}
			if b, err := os.ReadFile(want); err != nil || string(b) != content {
				t.Errorf("local copy = %q, %v; want %q", b, err, content)
			}
			if _, err := os.Stat(filepath.Join(localRoot, "2026", "IMG_0002.JPG")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("template path has a file (err %v); the copy belongs only at the server's path", err)
			}
			if len(u.calls) != 1 {
				t.Errorf("upload calls = %d, want 1", len(u.calls))
			}
			noAgentTemps(t, localRoot)
		})
	}
}

// TestUploadStreamCancelStopsBeforeHeldSidecars: the sidecars held back
// until after the walk honour ctx like the walk does -- a cancel during
// the last master's upload uploads no sidecar, and IngestCard returns the
// files already handled together with ctx's error.
func TestUploadStreamCancelStopsBeforeHeldSidecars(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeUploadCardFile(t, cardRoot, "A.JPG", "a", time.Time{})
	writeUploadCardFile(t, cardRoot, "A.xmp", "<xmp/>", time.Time{})
	writeUploadCardFile(t, cardRoot, "B.JPG", "b", time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := &recordingUploader{beforeRead: func(o branchdam.UploadOptions) {
		if o.Filename == "B.JPG" {
			cancel()
		}
	}}
	res, err := newUploadEngine(u, filepath.Join(dir, "local")).IngestCard(ctx, cardRoot)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IngestCard err = %v, want context.Canceled", err)
	}
	for _, c := range u.calls {
		if strings.HasSuffix(c.Filename, ".xmp") {
			t.Errorf("sidecar %s uploaded after the cancel", c.Filename)
		}
	}
	if len(res.Files) != 2 {
		t.Errorf("result files = %d, want the 2 masters handled before the cancel", len(res.Files))
	}
}
