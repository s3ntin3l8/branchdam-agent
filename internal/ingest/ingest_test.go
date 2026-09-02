package ingest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

type fakeClient struct {
	calls []branchdam.NodeCreatedPayload
}

func (f *fakeClient) PostNodeCreated(_ context.Context, _ string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
	f.calls = append(f.calls, payload)
	return &branchdam.EventResponse{EventID: "evt-" + payload.NodeUUID}, nil
}

type fakeUploaderClient struct {
	fakeClient
	uploadCalls []branchdam.UploadOptions
	uploadResp  *branchdam.UploadResponse
	uploadErr   error
}

func (f *fakeUploaderClient) Upload(_ context.Context, r io.Reader, opts branchdam.UploadOptions) (*branchdam.UploadResponse, error) {
	f.uploadCalls = append(f.uploadCalls, opts)
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	hasher := blake3.New()
	n, _ := io.Copy(hasher, r)
	hash := fmt.Sprintf("%x", hasher.Sum(nil))
	if f.uploadResp != nil {
		resp := *f.uploadResp
		if resp.Blake3Hash == "" {
			resp.Blake3Hash = hash
		}
		if resp.BytesWritten == 0 {
			resp.BytesWritten = n
		}
		return &resp, nil
	}
	return &branchdam.UploadResponse{
		NodeUUID:     "018f9999-upload-node-uuid",
		Status:       "UPLOADED",
		BytesWritten: n,
		Blake3Hash:   hash,
		RelativePath: "2026/2026-08-29/" + opts.Filename,
	}, nil
}

func newTestEngine(t *testing.T, client nodeCreator, archiveRoot, localRoot string) *Engine {
	t.Helper()
	return &Engine{
		Client:  client,
		AgentID: "test-agent",
		Ingest: config.IngestConfig{
			ArchiveRoot:   archiveRoot,
			LocalEditRoot: localRoot,
			PathTemplate:  "{original_name}",
		},
		Mappings: []config.PathMapping{
			{WorkstationPath: archiveRoot, ContainerPath: "/storage/archive"},
		},
		Exiftool: &Exiftool{}, // no exiftool -- fast_hash-only indexing path
		Now:      time.Now,
		NewNodeUUID: func() (string, error) {
			return "0000000-fake-uuid", nil
		},
	}
}

func TestIngestCardBasicFlow(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("file error: %v", fr.Err)
	}
	if fr.Skipped {
		t.Fatal("expected a non-sidecar file to be submitted, got Skipped=true")
	}
	if fr.EventID == "" {
		t.Error("expected an EventID after submission")
	}
	if len(client.calls) != 1 {
		t.Fatalf("got %d PostNodeCreated calls, want 1", len(client.calls))
	}
	payload := client.calls[0]
	if payload.FilePath != "/storage/archive/IMG_0001.jpg" {
		t.Errorf("FilePath = %q, want /storage/archive/IMG_0001.jpg", payload.FilePath)
	}
	if payload.FastHash == nil || payload.FullHash == nil {
		t.Error("expected FastHash and FullHash to be set")
	}
	if payload.FilenameStem == nil || *payload.FilenameStem != "img_0001" {
		t.Errorf("FilenameStem = %v, want img_0001", payload.FilenameStem)
	}

	// Both destination copies exist with matching content.
	archiveContent, err := os.ReadFile(fr.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	localContent, err := os.ReadFile(fr.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(archiveContent) != "fake-jpeg-bytes" || string(localContent) != "fake-jpeg-bytes" {
		t.Error("destination content mismatch")
	}
}

func TestIngestCardSidecarFilesAreCopiedButNotSubmitted(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "sidecar.xmp"), []byte("<xmp/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("file error: %v", fr.Err)
	}
	if !fr.Skipped {
		t.Error("expected .xmp sidecar to be Skipped (no event submitted)")
	}
	if len(client.calls) != 0 {
		t.Errorf("got %d PostNodeCreated calls for a sidecar file, want 0", len(client.calls))
	}
	if _, err := os.Stat(fr.ArchivePath); err != nil {
		t.Errorf("sidecar was not copied to archive destination: %v", err)
	}
	if _, err := os.Stat(fr.LocalPath); err != nil {
		t.Errorf("sidecar was not copied to local destination: %v", err)
	}
}

func TestIngestCardMissingPathMappingFailsThatFile(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "photo.jpg"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Mappings = nil // no mapping configured

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	fr := res.Files[0]
	if fr.Err == nil {
		t.Fatal("expected an error when no PathMapping covers the archive root")
	}
	if len(client.calls) != 0 {
		t.Error("must not submit an event when the container path can't be constructed")
	}
}

func TestIngestCardCollisionAutoSuffixing(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	dir1 := filepath.Join(cardRoot, "DCIM", "100MSDCF")
	dir2 := filepath.Join(cardRoot, "DCIM", "101MSDCF")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}

	// Two distinct files with the exact same basename in different card folders
	if err := os.WriteFile(filepath.Join(dir1, "DSC0001.JPG"), []byte("photo-one-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir1, "DSC0001.xmp"), []byte("<xmp-one/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "DSC0001.JPG"), []byte("photo-two-different-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "DSC0001.xmp"), []byte("<xmp-two/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	e := newTestEngine(t, client, archiveRoot, localRoot)
	e.Ingest.PathTemplate = "{original_name}"

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 4 {
		t.Fatalf("got %d files, want 4", len(res.Files))
	}
	for _, f := range res.Files {
		if f.Err != nil {
			t.Fatalf("unexpected error for %s: %v", f.SourcePath, f.Err)
		}
	}

	// First photo and its sidecar
	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001.JPG")); err != nil {
		t.Errorf("DSC0001.JPG missing in archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001.xmp")); err != nil {
		t.Errorf("DSC0001.xmp missing in archive: %v", err)
	}

	// Second photo (suffixed) and its paired sidecar (suffixed to match)
	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001_2.JPG")); err != nil {
		t.Errorf("DSC0001_2.JPG missing in archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0001_2.xmp")); err != nil {
		t.Errorf("DSC0001_2.xmp missing in archive: %v", err)
	}

	data1, _ := os.ReadFile(filepath.Join(archiveRoot, "DSC0001.JPG"))
	data2, _ := os.ReadFile(filepath.Join(archiveRoot, "DSC0001_2.JPG"))
	if string(data1) != "photo-one-content" || string(data2) != "photo-two-different-content" {
		t.Errorf("content mismatch for suffixed files")
	}
}

func TestIngestCardReingestSkipIdentical(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), []byte("photo-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	archiveRoot := filepath.Join(dir, "archive")
	localRoot := filepath.Join(dir, "local")
	e := newTestEngine(t, client, archiveRoot, localRoot)

	// First ingest: writes and submits
	res1, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil || res1.Files[0].Err != nil {
		t.Fatalf("first ingest failed: %v, %v", err, res1.Files[0].Err)
	}
	if res1.Files[0].Skipped {
		t.Fatal("first ingest should not be skipped")
	}

	// Second ingest of same card: detects identical destination and skips
	res2, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil || res2.Files[0].Err != nil {
		t.Fatalf("second ingest failed: %v, %v", err, res2.Files[0].Err)
	}
	if !res2.Files[0].Skipped {
		t.Error("second ingest of identical file should be skipped")
	}
	if res2.Files[0].SkipReason != "already ingested (identical file exists at destination)" {
		t.Errorf("got skip reason %q", res2.Files[0].SkipReason)
	}
}

func TestIngestCardRequireUnbufferedFailsOnBufferedFloor(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "photo.jpg"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Ingest.RequireUnbuffered = true

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	// On tmpfs / Linux tmp dir, O_DIRECT returns EINVAL, so method is buffered_floor.
	if fr.ArchiveVerify.Method == VerifyMethodBufferedFloor || fr.LocalVerify.Method == VerifyMethodBufferedFloor {
		if fr.Err == nil {
			t.Error("expected error when RequireUnbuffered=true and method is buffered_floor")
		}
	}
}

func TestIngestCardUploadStream(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0042.jpg"), []byte("streaming-photo-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	uploader := &fakeUploaderClient{}
	localRoot := filepath.Join(dir, "local")
	e := NewEngine(uploader, "test-agent", config.IngestConfig{
		LocalEditRoot: localRoot,
		UploadStream:  true,
	}, nil)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("file error: %v", fr.Err)
	}

	if len(uploader.uploadCalls) != 1 {
		t.Fatalf("got %d upload calls, want 1", len(uploader.uploadCalls))
	}
	if uploader.uploadCalls[0].Filename != "IMG_0042.jpg" {
		t.Errorf("uploaded filename = %q, want IMG_0042.jpg", uploader.uploadCalls[0].Filename)
	}

	// Local file was written under LocalEditRoot + relativePath returned by server
	expectedLocal := filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0042.jpg")
	data, err := os.ReadFile(expectedLocal)
	if err != nil {
		t.Fatalf("read local copy %s: %v", expectedLocal, err)
	}
	if string(data) != "streaming-photo-data" {
		t.Errorf("local data = %q, want 'streaming-photo-data'", string(data))
	}

	if !fr.LocalVerify.Verified {
		t.Error("local copy was not verified")
	}
	if fr.NodeUUID != "018f9999-upload-node-uuid" {
		t.Errorf("fr.NodeUUID = %q", fr.NodeUUID)
	}
}

func TestIngestCardUploadStream_VerifyFailure(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0042.jpg"), []byte("streaming-photo-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	uploader := &fakeUploaderClient{
		uploadResp: &branchdam.UploadResponse{
			NodeUUID:     "018f9999-corrupt-hash",
			Status:       "UPLOADED",
			BytesWritten: 20,
			Blake3Hash:   "0000000000000000000000000000000000000000000000000000000000000000",
			RelativePath: "2026/2026-08-29/IMG_0042.jpg",
		},
	}
	localRoot := filepath.Join(dir, "local")
	e := NewEngine(uploader, "test-agent", config.IngestConfig{
		LocalEditRoot: localRoot,
		UploadStream:  true,
	}, nil)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	fr := res.Files[0]
	if fr.Err == nil {
		t.Fatal("expected error on hash mismatch, got nil")
	}

	// Local file should be removed on verification failure
	expectedLocal := filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0042.jpg")
	if _, err := os.Stat(expectedLocal); !os.IsNotExist(err) {
		t.Error("expected local file to be removed after verification failure")
	}
}

// TestIngestCardSkipsOSMetadata pins issue #100's #1 footgun: a card
// formatted by macOS leaves a .DS_Store in every directory; a card
// touched by Windows leaves Thumbs.db. Pre-#100, every such file became
// a media_nodes row and an exiftool fork. With #100 the walk short-
// circuits them and reports them as Skipped.
func TestIngestCardSkipsOSMetadata(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	subdir := filepath.Join(cardRoot, "DCIM", "100MSDCF")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real photo -- must be ingested, not skipped.
	if err := os.WriteFile(filepath.Join(subdir, "DSC0001.JPG"), []byte("photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// OS-metadata files the walk must skip, scattered in two
	// directories so the directory count vs. file count would be
	// visibly wrong if any leak through.
	for _, p := range []string{
		filepath.Join(cardRoot, ".DS_Store"),
		filepath.Join(cardRoot, "._DSC0001.JPG"), // AppleDouble
		filepath.Join(cardRoot, ".Spotlight-V100"),
		filepath.Join(subdir, "Thumbs.db"),
		filepath.Join(subdir, ".DS_Store"),
	} {
		if err := os.WriteFile(p, []byte("os-junk"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	// Walk produces one FileResult per non-dir entry, including the
	// OS-metadata ones (so the operator sees them in the result). The
	// real assertion is on Skipped + EventID + client.calls.
	if len(res.Files) != 6 {
		t.Fatalf("got %d files, want 6 (1 photo + 5 OS-metadata)", len(res.Files))
	}

	// Exactly one real photo gets submitted; the 5 OS-metadata files
	// are Skipped and have no EventID.
	if len(client.calls) != 1 {
		t.Fatalf("got %d PostNodeCreated calls, want 1 (only the real photo)", len(client.calls))
	}
	real, junk := 0, 0
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Fatalf("unexpected error for %s: %v", fr.SourcePath, fr.Err)
		}
		if fr.Skipped {
			junk++
			if fr.EventID != "" {
				t.Errorf("OS-metadata file %s was Skipped but has an EventID (%s) -- it must not have been submitted", filepath.Base(fr.SourcePath), fr.EventID)
			}
			if !strings.HasPrefix(fr.SkipReason, "OS metadata:") {
				t.Errorf("OS-metadata file %s has wrong skip reason %q", filepath.Base(fr.SourcePath), fr.SkipReason)
			}
		} else {
			real++
			if fr.EventID == "" {
				t.Errorf("real file %s missing EventID", filepath.Base(fr.SourcePath))
			}
		}
	}
	if real != 1 || junk != 5 {
		t.Errorf("got real=%d junk=%d, want 1 and 5", real, junk)
	}

	// Destination tree must not contain the OS-metadata files: skipping
	// means the walk never invokes ingestFile, so DualWrite never ran.
	archiveRoot := filepath.Join(dir, "archive")
	for _, name := range []string{".DS_Store", "Thumbs.db", "._DSC0001.JPG", ".Spotlight-V100"} {
		if _, err := os.Stat(filepath.Join(archiveRoot, name)); err == nil {
			t.Errorf("OS-metadata file %s leaked into archive destination", name)
		}
	}
}

// TestIngestCardAllowedExtensionsFilter pins the M5/#100 extension
// allowlist: a non-empty Ingest.AllowedExtensions narrows the walk to
// only KNOWN-EXTENSION files whose extension is on the list, case-
// insensitively. Files with no extension are NOT filtered by the
// allowlist (Hermes review on #127): positive identification via
// ingestFile's isImageExt/isVideoExt is the safer default than
// silently dropping them.
//
// An empty list (the default, the regression guard for older
// configs) accepts everything the OS-metadata skip doesn't rule out.
func TestIngestCardAllowedExtensionsFilter(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"a.jpg", // matches
		"b.JPG", // matches (case)
		"c.mp4", // matches
		"d.txt", // filtered out
		"e.png", // filtered out
		"f",     // no ext -- NOT filtered by allowlist, falls through
	} {
		if err := os.WriteFile(filepath.Join(cardRoot, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	e.Ingest.AllowedExtensions = []string{"jpg", "MP4"}

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}

	// All 6 files appear in res.Files (the walk still visits them).
	// 3 matching extensions are submitted. 2 wrong-extension files
	// are filtered. 1 extension-less file falls through and is
	// submitted.
	if len(res.Files) != 6 {
		t.Fatalf("got %d files, want 6", len(res.Files))
	}
	if len(client.calls) != 4 {
		t.Errorf("got %d PostNodeCreated calls, want 4 (jpg/JPG/mp4 + extension-less f)", len(client.calls))
	}
	submitted, filtered := 0, 0
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Fatalf("unexpected error for %s: %v", fr.SourcePath, fr.Err)
		}
		base := filepath.Base(fr.SourcePath)
		switch base {
		case "a.jpg", "b.JPG", "c.mp4", "f":
			// f (extension-less) falls through the allowlist per
			// Hermes review on #127. It still goes through
			// ingestFile normally.
			if fr.Skipped {
				t.Errorf("%s must NOT be skipped (matches allowlist, or no extension)", base)
			}
			if fr.EventID == "" {
				t.Errorf("%s missing EventID", base)
			}
			submitted++
		default:
			if !fr.Skipped {
				t.Errorf("%s must be skipped (not in allowlist)", base)
			}
			if fr.EventID != "" {
				t.Errorf("%s Skipped but has an EventID", base)
			}
			if !strings.Contains(fr.SkipReason, "extension") {
				t.Errorf("%s skip reason = %q, want something mentioning extension", base, fr.SkipReason)
			}
			filtered++
		}
	}
	if submitted != 4 || filtered != 2 {
		t.Errorf("got submitted=%d filtered=%d, want 4 and 2", submitted, filtered)
	}

	// Rejected files (wrong-extension) must not have been copied.
	// The extension-less file f IS expected to be copied -- it falls
	// through the allowlist per Hermes review on #127.
	archiveRoot := filepath.Join(dir, "archive")
	for _, name := range []string{"d.txt", "e.png"} {
		if _, err := os.Stat(filepath.Join(archiveRoot, name)); err == nil {
			t.Errorf("rejected file %s leaked into archive destination", name)
		}
	}
}

// TestIngestCardAllowedExtensionsAcceptsAllWhenEmpty is the backward-
// compat guard: a config that never mentions AllowedExtensions must
// behave exactly like pre-#100 (everything not matching an OS-metadata
// rule gets ingested).
func TestIngestCardAllowedExtensionsAcceptsAllWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jpg", "b.txt", "c.unknown"} {
		if err := os.WriteFile(filepath.Join(cardRoot, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	if len(e.Ingest.AllowedExtensions) != 0 {
		t.Fatalf("test setup wrong: AllowedExtensions=%v, want empty", e.Ingest.AllowedExtensions)
	}

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(client.calls) != 3 {
		t.Errorf("got %d PostNodeCreated calls, want 3 (no allowlist = accept all)", len(client.calls))
	}
	for _, fr := range res.Files {
		if fr.Skipped {
			t.Errorf("%s should NOT be skipped when AllowedExtensions is empty", filepath.Base(fr.SourcePath))
		}
	}
}
