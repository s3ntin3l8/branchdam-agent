package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// captureSlog swaps slog.Default() for a JSON-handler writing to buf for
// the duration of the test, restoring the prior default on cleanup.
// Mirrors internal/config's captureSlog helper so the chtimes-logging
// assertions can be done with the same level/msg/path/destination/error
// shape config_test.go uses.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf
}

// findCHtimesWarn scans a captured slog buffer for a chtimes-failure Warn
// record matching the given destination. Returns the decoded record so the
// caller can assert on level/path/source/destination/err fields; nil if
// no matching record was emitted (which is the failure mode the test is
// trying to pin down -- a silently-swallowed error).
func findCHtimesWarn(t *testing.T, buf *bytes.Buffer, destination string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("slog emitted a non-JSON line: %q", line)
		}
		if rec["msg"] == "ingest: failed to preserve source mtime on destination" &&
			rec["destination"] == destination {
			return rec
		}
	}
	return nil
}

// TestIngestCardChtimesFailureIsLogged is the issue #103 contract: when
// os.Chtimes fails on a destination (the soft-mtime contract the prune-
// safety half of invariant #8 depends on), ingest must NOT fail -- the
// file is on disk, only its mtime preservation is best-effort -- but the
// failure MUST be surfaced via slog.Warn carrying source path,
// destination path, and the underlying error. Silent swallowing is the
// regression we're guarding against.
//
// We inject the failure via the cHtimesFn indirection (mirroring
// writer.go's syncParentDirFn pattern) rather than trying to make a real
// os.Chtimes call fail in a unit test -- chmod-based approaches are
// racy (they need to land between DualWrite and the Chtimes call) and
// platform-dependent (0o500 vs EACCES behavior differs across Linux/
// macOS/Windows).
func TestIngestCardChtimesFailureIsLogged(t *testing.T) {
	origChtimes := cHtimesFn
	t.Cleanup(func() { cHtimesFn = origChtimes })
	cHtimesFn = func(path string, atime, mtime time.Time) error {
		return &os.PathError{Op: "chtimes", Path: path, Err: syscall.ENOENT}
	}

	logBuf := captureSlog(t)

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

	// Soft contract: chtimes failure must NOT fail the ingest. The file
	// is on disk, the hashes verified, the event submitted.
	if fr.Err != nil {
		t.Fatalf("ingest must NOT fail on chtimes error (soft mtime contract); got %v", fr.Err)
	}
	if fr.EventID == "" {
		t.Error("expected an EventID -- ingest should have proceeded past the failed chtimes")
	}

	// Both destinations must have a Warn emitted with source +
	// destination + err. We expect two (one for archive, one for local)
	// because the production code calls Chtimes twice per file.
	archiveWarn := findCHtimesWarn(t, logBuf, filepath.Join(dir, "archive", "IMG_0001.jpg"))
	if archiveWarn == nil {
		t.Fatalf("expected slog.Warn for archive destination; got log: %s", logBuf.String())
	}
	if archiveWarn["level"] != "WARN" {
		t.Errorf("archive warn level = %v, want WARN", archiveWarn["level"])
	}
	if archiveWarn["source"] != filepath.Join(cardRoot, "IMG_0001.jpg") {
		t.Errorf("archive warn source = %v", archiveWarn["source"])
	}
	errMsg, ok := archiveWarn["err"].(string)
	if !ok || !strings.Contains(errMsg, "no such file") {
		t.Errorf("archive warn err = %v, want something containing 'no such file'", archiveWarn["err"])
	}

	localWarn := findCHtimesWarn(t, logBuf, filepath.Join(dir, "local", "IMG_0001.jpg"))
	if localWarn == nil {
		t.Fatalf("expected slog.Warn for local destination; got log: %s", logBuf.String())
	}
	if localWarn["level"] != "WARN" {
		t.Errorf("local warn level = %v, want WARN", localWarn["level"])
	}
}

// TestIngestCardChtimesSuccessEmitsNoWarn is the regression guard for
// the OTHER direction: a healthy os.Chtimes call must not emit the
// warning. We install the real os.Chtimes and assert the buffer
// contains no chtimes record at all.
func TestIngestCardChtimesSuccessEmitsNoWarn(t *testing.T) {
	// Save/restore-through-t.Cleanup for symmetry with the sibling tests
	// that DO substitute cHtimesFn (Hermes review note on #130).
	origChtimes := cHtimesFn
	t.Cleanup(func() { cHtimesFn = origChtimes })
	cHtimesFn = os.Chtimes

	logBuf := captureSlog(t)

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
	if len(res.Files) != 1 || res.Files[0].Err != nil {
		t.Fatalf("ingest should have succeeded: %+v", res.Files)
	}

	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "ingest: failed to preserve source mtime on destination" {
			t.Errorf("did not expect a chtimes-failure warning on a successful ingest; got: %s", line)
		}
	}
}

// TestIngestCardUploadStreamChtimesFailureIsLogged is the upload-stream
// twin of TestIngestCardChtimesFailureIsLogged: the upload path only
// writes a local copy (archive lands server-side), so there is exactly
// one os.Chtimes call to assert on. Same soft contract: failure must
// log, not fail.
func TestIngestCardUploadStreamChtimesFailureIsLogged(t *testing.T) {
	origChtimes := cHtimesFn
	t.Cleanup(func() { cHtimesFn = origChtimes })
	cHtimesFn = func(path string, atime, mtime time.Time) error {
		return &os.PathError{Op: "chtimes", Path: path, Err: syscall.ENOENT}
	}

	logBuf := captureSlog(t)

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
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("upload-stream ingest must NOT fail on chtimes error; got %v", fr.Err)
	}
	if fr.NodeUUID != "018f9999-upload-node-uuid" {
		t.Errorf("fr.NodeUUID = %q, want %q", fr.NodeUUID, "018f9999-upload-node-uuid")
	}

	wantDest := filepath.Join(localRoot, "2026", "2026-08-29", "IMG_0042.jpg")
	warn := findCHtimesWarn(t, logBuf, wantDest)
	if warn == nil {
		t.Fatalf("expected slog.Warn for local destination %q; got log: %s", wantDest, logBuf.String())
	}
	if warn["level"] != "WARN" {
		t.Errorf("warn level = %v, want WARN", warn["level"])
	}
	if warn["source"] != filepath.Join(cardRoot, "IMG_0042.jpg") {
		t.Errorf("warn source = %v", warn["source"])
	}
}

type fakeCheckContentClient struct {
	fakeClient
	checkCalls []struct{ FastHash, FullHash string }
	checkFunc  func(ctx context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error)
}

func (f *fakeCheckContentClient) CheckContent(ctx context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
	f.checkCalls = append(f.checkCalls, struct{ FastHash, FullHash string }{fastHash, fullHash})
	if f.checkFunc != nil {
		return f.checkFunc(ctx, fastHash, fullHash)
	}
	return branchdam.ContentCheckResult{Found: false}, nil
}

func TestIngestFileDedupPreFlight(t *testing.T) {
	t.Run("confirmed duplicate skips DualWrite and sets ExistingNodeUUID", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0001.JPG"), []byte("duplicate-image-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
				if fullHash == "" {
					// Fast-hash pre-screen hit
					return branchdam.ContentCheckResult{Found: true}, nil
				}
				// Full-hash confirmation hit
				return branchdam.ContentCheckResult{
					Found:          true,
					NodeUUID:       "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
					FilePath:       "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC0001.JPG",
					LifecycleState: "ACTIVE",
				}, nil
			},
		}

		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if !fr.Skipped {
			t.Errorf("expected Skipped=true, got false")
		}
		if fr.ExistingNodeUUID != "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60" {
			t.Errorf("ExistingNodeUUID = %q, want 0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60", fr.ExistingNodeUUID)
		}
		wantReason := "duplicate: already in library as node 0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60 at /storage/archive/2026/2026-07-15_ILCE-7M4/DSC0001.JPG"
		if fr.SkipReason != wantReason {
			t.Errorf("SkipReason = %q, want %q", fr.SkipReason, wantReason)
		}

		// Zero bytes written to archiveRoot or localRoot
		if _, err := os.Stat(fr.ArchivePath); !os.IsNotExist(err) {
			t.Errorf("archive file should not exist, err=%v", err)
		}
		if _, err := os.Stat(fr.LocalPath); !os.IsNotExist(err) {
			t.Errorf("local file should not exist, err=%v", err)
		}
		if len(client.calls) != 0 {
			t.Errorf("expected 0 PostNodeCreated calls, got %d", len(client.calls))
		}
		if len(client.checkCalls) != 2 {
			t.Fatalf("expected 2 CheckContent calls (fast then full), got %d", len(client.checkCalls))
		}
		if client.checkCalls[0].FullHash != "" {
			t.Errorf("first check should have empty fullHash, got %q", client.checkCalls[0].FullHash)
		}
		if client.checkCalls[1].FullHash == "" {
			t.Errorf("second check should have non-empty fullHash")
		}
	})

	t.Run("fast-hash false positive proceeds with DualWrite", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0002.JPG"), []byte("unique-image-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
				if fullHash == "" {
					// Fast-hash collision / false positive
					return branchdam.ContentCheckResult{Found: true}, nil
				}
				// Full-hash mismatch -> not found
				return branchdam.ContentCheckResult{Found: false}, nil
			},
		}

		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped on false positive, got Skipped=true")
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
		if _, err := os.Stat(fr.ArchivePath); err != nil {
			t.Errorf("archive file missing: %v", err)
		}
		if _, err := os.Stat(fr.LocalPath); err != nil {
			t.Errorf("local file missing: %v", err)
		}
	})

	t.Run("fast-hash not found skips full-hash check and proceeds with DualWrite", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0003.JPG"), []byte("new-image-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, fastHash, fullHash string) (branchdam.ContentCheckResult, error) {
				return branchdam.ContentCheckResult{Found: false}, nil
			},
		}

		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped, got Skipped=true")
		}
		if len(client.checkCalls) != 1 {
			t.Errorf("expected only 1 check call (fastHash), got %d", len(client.checkCalls))
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
	})

	t.Run("server network failure fails open and proceeds with DualWrite", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0004.JPG"), []byte("failopen-image-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{
			checkFunc: func(_ context.Context, _, _ string) (branchdam.ContentCheckResult, error) {
				return branchdam.ContentCheckResult{}, fmt.Errorf("connection refused")
			},
		}

		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped on network failure, got Skipped=true")
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
	})

	t.Run("disabled preflight skips CheckContent entirely", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0005.JPG"), []byte("disabled-preflight-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)
		e.Ingest.PreflightTimeoutSecs = -1

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped, got Skipped=true")
		}
		if len(client.checkCalls) != 0 {
			t.Errorf("expected 0 CheckContent calls when disabled, got %d", len(client.checkCalls))
		}
	})
}
