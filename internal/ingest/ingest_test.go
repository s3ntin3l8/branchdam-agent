package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/hashing"
)

type fakeClient struct {
	calls     []branchdam.NodeCreatedPayload
	uuidCalls []string
}

func (f *fakeClient) PostNodeCreated(_ context.Context, _ string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
	f.calls = append(f.calls, payload)
	return &branchdam.EventResponse{EventID: "evt-" + payload.NodeUUID}, nil
}

func (f *fakeClient) PostNodeCreatedWithUUID(_ context.Context, _ string, payload branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error) {
	f.calls = append(f.calls, payload)
	f.uuidCalls = append(f.uuidCalls, eventUUID)
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
	// sourcePathHash is SHA-256 of the card-side path, the same digest the
	// streaming upload sends as X-Source-Path-Hash.
	wantSrcHash := sha256.Sum256([]byte(filepath.Join(cardRoot, "IMG_0001.jpg")))
	if payload.SourcePathHash == nil || *payload.SourcePathHash != hex.EncodeToString(wantSrcHash[:]) {
		t.Errorf("SourcePathHash = %v, want sha256 of the source path %x", payload.SourcePathHash, wantSrcHash)
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
	// The mapping is checked before the copy: nothing may be left at
	// either destination for a later run to mistake for an ingested file.
	for _, root := range []string{filepath.Join(dir, "archive"), filepath.Join(dir, "local")} {
		if _, err := os.Stat(filepath.Join(root, "photo.jpg")); !os.IsNotExist(err) {
			t.Errorf("%s: a copy was written although the file could not be registered (stat err=%v)", root, err)
		}
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
	e.Queue = openStoreT(t)

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
	// The skip rests on the queue.db row the first run wrote before its
	// POST, not on the files alone.
	if want := "already ingested (identical file exists at destination); registered as node 0000000-fake-uuid"; res2.Files[0].SkipReason != want {
		t.Errorf("got skip reason %q, want %q", res2.Files[0].SkipReason, want)
	}
	if len(client.calls) != 1 {
		t.Errorf("got %d EVENT_NODE_CREATED posts across both runs, want 1", len(client.calls))
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
	forceBufferedVerify(t)

	// Twice: the first run must not leave copies behind that a second run
	// would then pass as "already ingested" without the required verify.
	for run := 1; run <= 2; run++ {
		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("run %d: IngestCard: %v", run, err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("run %d: got %d files, want 1", run, len(res.Files))
		}
		fr := res.Files[0]
		if fr.Err == nil {
			t.Fatalf("run %d: expected an error when RequireUnbuffered=true and verify used the buffered floor (skipped=%v %q)", run, fr.Skipped, fr.SkipReason)
		}
		for _, p := range []string{fr.ArchivePath, fr.LocalPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("run %d: %s left in place after the required unbuffered verify failed (stat err=%v)", run, p, err)
			}
		}
	}
	if len(client.calls) != 0 {
		t.Errorf("got %d EVENT_NODE_CREATED posts, want 0", len(client.calls))
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
	wantSrcHash := sha256.Sum256([]byte(filepath.Join(cardRoot, "IMG_0042.jpg")))
	if uploader.uploadCalls[0].SourcePathHash != hex.EncodeToString(wantSrcHash[:]) {
		t.Errorf("sourcePathHash = %q, want %q", uploader.uploadCalls[0].SourcePathHash, hex.EncodeToString(wantSrcHash[:]))
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
	chtimesErr := errors.New("simulated chtimes failure")
	cHtimesFn = func(path string, atime, mtime time.Time) error {
		return &os.PathError{Op: "chtimes", Path: path, Err: chtimesErr}
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
	if !ok || !strings.Contains(errMsg, chtimesErr.Error()) {
		t.Errorf("archive warn err = %v, want something containing %q", archiveWarn["err"], chtimesErr)
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
	checkCalls []branchdam.ContentCheckQuery
	checkFunc  func(ctx context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error)
}

func (f *fakeCheckContentClient) CheckContent(ctx context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
	f.checkCalls = append(f.checkCalls, q)
	if f.checkFunc != nil {
		return f.checkFunc(ctx, q)
	}
	return branchdam.ContentCheckResult{}, nil
}

// prescreenServer answers check-content the way branchDAM (#538) does: a
// fast-only request gets {found:false, possibleMatch}, never node details;
// a full-hash request gets the definitive answer, with possibleMatch
// mirroring found.
func prescreenServer(possible bool, full branchdam.ContentCheckResult) func(context.Context, branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
	return func(_ context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		if q.FullHash == "" {
			return branchdam.ContentCheckResult{Found: false, PossibleMatch: possible}, nil
		}
		full.PossibleMatch = full.Found
		return full, nil
	}
}

// oldServer answers check-content the way a server predating the fast-hash
// pre-screen does: a request without fullHash is 400, a full-hash request
// is answered (found + details).
func oldServer(full branchdam.ContentCheckResult) func(context.Context, branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
	return func(_ context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		if q.FullHash == "" {
			return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: http.StatusBadRequest, Body: `{"title":"Bad Request","status":400,"detail":"fullHash must be 64 hexadecimal characters"}`}
		}
		return full, nil
	}
}

func liveDuplicate(state string) branchdam.ContentCheckResult {
	return branchdam.ContentCheckResult{
		Found:          true,
		NodeUUID:       "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		FilePath:       "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC0001.JPG",
		LifecycleState: state,
	}
}

// writeCard creates cardRoot with the given name -> content files.
func writeCard(t *testing.T, cardRoot string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(cardRoot, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// wantHashes returns the FastHash and BLAKE3 full hash of p, computed the
// way the server's columns are.
func wantHashes(t *testing.T, p string) (fast, full string) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fast, err = hashing.FastHash(f, fi.Size())
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	return fast, blake3Hex(t, p)
}

func TestIngestFileDedupPreFlight(t *testing.T) {
	t.Run("possibleMatch then live full-hash match skips and removes both copies", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0001.JPG": "duplicate-image-bytes"})
		srcPath := filepath.Join(cardRoot, "DSC0001.JPG")
		wantFast, wantFull := wantHashes(t, srcPath)

		client := &fakeCheckContentClient{checkFunc: prescreenServer(true, liveDuplicate("ACTIVE"))}
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
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if !fr.Skipped {
			t.Errorf("expected Skipped=true, got false")
		}
		if fr.ExistingNodeUUID != "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60" {
			t.Errorf("ExistingNodeUUID = %q", fr.ExistingNodeUUID)
		}
		wantReason := "duplicate: already in library as node 0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60 at /storage/archive/2026/2026-07-15_ILCE-7M4/DSC0001.JPG"
		if fr.SkipReason != wantReason {
			t.Errorf("SkipReason = %q, want %q", fr.SkipReason, wantReason)
		}
		if _, err := os.Stat(fr.ArchivePath); !os.IsNotExist(err) {
			t.Errorf("archive copy of a duplicate must be removed, err=%v", err)
		}
		if _, err := os.Stat(fr.LocalPath); !os.IsNotExist(err) {
			t.Errorf("local copy of a duplicate must be removed, err=%v", err)
		}
		if len(client.calls) != 0 {
			t.Errorf("expected 0 PostNodeCreated calls, got %d", len(client.calls))
		}
		if len(client.checkCalls) != 2 {
			t.Fatalf("expected 2 CheckContent calls (pre-screen then confirm), got %d", len(client.checkCalls))
		}
		pre, confirm := client.checkCalls[0], client.checkCalls[1]
		if pre.FastHash != wantFast || pre.SizeBytes != int64(len("duplicate-image-bytes")) || pre.FullHash != "" {
			t.Errorf("pre-screen query = %+v, want fastHash=%s sizeBytes=%d and no fullHash", pre, wantFast, len("duplicate-image-bytes"))
		}
		if confirm.FullHash != wantFull {
			t.Errorf("confirm fullHash = %q, want %q", confirm.FullHash, wantFull)
		}
	})

	t.Run("dedup uses the in-flight hashes and never re-reads the card", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0001.JPG": "read-once-bytes"})
		srcPath := filepath.Join(cardRoot, "DSC0001.JPG")
		_, wantFull := wantHashes(t, srcPath)

		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		var copiesExistedAtCheck bool
		client := &fakeCheckContentClient{}
		client.checkFunc = func(ctx context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
			if q.FullHash == "" {
				// The check runs after the single copy: both copies exist.
				// Pull the card file away; any later read of it for
				// hashing would fail.
				_, aErr := os.Stat(filepath.Join(archiveRoot, "DSC0001.JPG"))
				_, lErr := os.Stat(filepath.Join(localRoot, "DSC0001.JPG"))
				copiesExistedAtCheck = aErr == nil && lErr == nil
				if err := os.Remove(srcPath); err != nil {
					t.Fatal(err)
				}
			}
			return prescreenServer(true, liveDuplicate("ACTIVE"))(ctx, q)
		}
		e := newTestEngine(t, client, archiveRoot, localRoot)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if !copiesExistedAtCheck {
			t.Error("content check ran before the copy: the card would be read more than once")
		}
		if len(client.checkCalls) != 2 || client.checkCalls[1].FullHash != wantFull {
			t.Fatalf("check calls = %+v, want a confirm carrying fullHash %s", client.checkCalls, wantFull)
		}
		if fr := res.Files[0]; !fr.Skipped || fr.Err != nil {
			t.Errorf("Skipped=%v Err=%v, want a duplicate skip", fr.Skipped, fr.Err)
		}
	})

	t.Run("HIDDEN node counts as live and is skipped", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0001.JPG": "hidden-node-bytes"})

		client := &fakeCheckContentClient{checkFunc: prescreenServer(true, liveDuplicate("HIDDEN"))}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if !fr.Skipped {
			t.Errorf("a HIDDEN duplicate must be skipped, got Skipped=false")
		}
		if len(client.calls) != 0 {
			t.Errorf("expected 0 PostNodeCreated calls, got %d", len(client.calls))
		}
	})

	t.Run("full-hash match on a non-live node proceeds", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0004.JPG": "archived-node-content"})

		client := &fakeCheckContentClient{checkFunc: prescreenServer(true, liveDuplicate("ARCHIVED"))}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Skipped || fr.Err != nil {
			t.Errorf("ARCHIVED duplicate: Skipped=%v Err=%v, want ingested", fr.Skipped, fr.Err)
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
		if _, err := os.Stat(fr.ArchivePath); err != nil {
			t.Errorf("archive copy missing: %v", err)
		}
	})

	t.Run("a match on this file's own archive path is not a duplicate", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0009.JPG": "self-indexed-bytes"})
		archiveRoot := filepath.Join(dir, "archive")

		client := &fakeCheckContentClient{}
		client.checkFunc = func(ctx context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
			// The server already indexed the copy this run just wrote:
			// the match is at this file's own archive container path
			// (newTestEngine maps archiveRoot to /storage/archive).
			if _, err := os.Stat(filepath.Join(archiveRoot, "DSC0009.JPG")); err != nil {
				t.Fatalf("archive copy not found at check time: %v", err)
			}
			self := liveDuplicate("ACTIVE")
			self.FilePath = "/storage/archive/DSC0009.JPG"
			return prescreenServer(true, self)(ctx, q)
		}
		e := newTestEngine(t, client, archiveRoot, filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Skipped || fr.Err != nil {
			t.Fatalf("Skipped=%v Err=%v, want ingested", fr.Skipped, fr.Err)
		}
		if _, err := os.Stat(fr.ArchivePath); err != nil {
			t.Errorf("own archive copy must not be removed: %v", err)
		}
	})

	t.Run("possibleMatch with a full-hash miss proceeds", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0002.JPG": "unique-image-bytes"})

		client := &fakeCheckContentClient{checkFunc: prescreenServer(true, branchdam.ContentCheckResult{Found: false})}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped on a fast-hash false positive")
		}
		if len(client.checkCalls) != 2 {
			t.Errorf("expected 2 check calls, got %d", len(client.checkCalls))
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

	t.Run("no possibleMatch never sends the full hash", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0003.JPG": "new-image-bytes"})

		client := &fakeCheckContentClient{checkFunc: prescreenServer(false, branchdam.ContentCheckResult{})}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Skipped || fr.Err != nil {
			t.Errorf("Skipped=%v Err=%v, want ingested", fr.Skipped, fr.Err)
		}
		if len(client.checkCalls) != 1 || client.checkCalls[0].FullHash != "" {
			t.Errorf("check calls = %+v, want only the fast pre-screen", client.checkCalls)
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
	})

	t.Run("older server's 400 falls back to the full-hash lookup without latching", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{
			"DSC0001.JPG": "file1",
			"DSC0002.JPG": "file2",
			"DSC0003.JPG": "file3",
		})

		client := &fakeCheckContentClient{checkFunc: oldServer(liveDuplicate("ACTIVE"))}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 3 {
			t.Fatalf("got %d files, want 3", len(res.Files))
		}
		for _, fr := range res.Files {
			if fr.Err != nil || !fr.Skipped || fr.ExistingNodeUUID == "" {
				t.Errorf("%s: Skipped=%v Err=%v, want a duplicate skip via the full-hash lookup", fr.SourcePath, fr.Skipped, fr.Err)
			}
		}
		// One rejected pre-screen, then a full-hash lookup per file: the
		// 400 switches the pass to full-hash lookups, it doesn't disable
		// dedup.
		if len(client.checkCalls) != 4 {
			t.Fatalf("expected 4 check calls (1 rejected pre-screen + 3 full-hash lookups), got %d: %+v", len(client.checkCalls), client.checkCalls)
		}
		if client.checkCalls[0].FullHash != "" {
			t.Errorf("first call should be the fast pre-screen, got %+v", client.checkCalls[0])
		}
		for i, q := range client.checkCalls[1:] {
			if q.FullHash == "" {
				t.Errorf("call %d after the 400 has no fullHash: %+v", i+1, q)
			}
		}
		if len(client.calls) != 0 {
			t.Errorf("expected 0 PostNodeCreated calls, got %d", len(client.calls))
		}
	})

	t.Run("400 on the full-hash confirm fails open without latching", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0001.JPG": "file1", "DSC0002.JPG": "file2"})

		client := &fakeCheckContentClient{checkFunc: func(_ context.Context, q branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
			if q.FullHash == "" {
				return branchdam.ContentCheckResult{PossibleMatch: true}, nil
			}
			return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: http.StatusBadRequest, Body: "bad request"}
		}}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		for _, fr := range res.Files {
			if fr.Skipped || fr.Err != nil {
				t.Errorf("%s: Skipped=%v Err=%v, want ingested", fr.SourcePath, fr.Skipped, fr.Err)
			}
		}
		if len(client.checkCalls) != 4 {
			t.Errorf("expected 4 check calls (both files fully checked), got %d", len(client.checkCalls))
		}
	})

	t.Run("server 404 latches and skips the check for remaining files", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{
			"DSC0001.JPG": "file1",
			"DSC0002.JPG": "file2",
			"DSC0003.JPG": "file3",
		})

		client := &fakeCheckContentClient{checkFunc: func(context.Context, branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
			return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: http.StatusNotFound, Body: "404 page not found"}
		}}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 3 {
			t.Fatalf("got %d files, want 3", len(res.Files))
		}
		for _, fr := range res.Files {
			if fr.Skipped {
				t.Errorf("file %s was skipped", fr.SourcePath)
			}
		}
		if len(client.checkCalls) != 1 {
			t.Errorf("expected exactly 1 check call before latching, got %d", len(client.checkCalls))
		}
		if len(client.calls) != 3 {
			t.Errorf("expected 3 PostNodeCreated calls, got %d", len(client.calls))
		}
	})

	t.Run("server network failure fails open and proceeds", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0004.JPG": "failopen-image-bytes"})

		client := &fakeCheckContentClient{checkFunc: func(context.Context, branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
			return branchdam.ContentCheckResult{}, fmt.Errorf("connection refused")
		}}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected file to NOT be skipped on network failure")
		}
		if len(client.calls) != 1 {
			t.Errorf("expected 1 PostNodeCreated call, got %d", len(client.calls))
		}
	})

	t.Run("disabled preflight skips CheckContent entirely", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		writeCard(t, cardRoot, map[string]string{"DSC0005.JPG": "disabled-preflight-bytes"})

		client := &fakeCheckContentClient{}
		e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
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

	t.Run("AlreadyIngested copies the server does not know are registered, not skipped", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		content := []byte("already-ingested-image-bytes")
		srcPath := filepath.Join(cardRoot, "DSC0006.JPG")
		if err := os.WriteFile(srcPath, content, 0o644); err != nil {
			t.Fatal(err)
		}
		srcInfo, err := os.Stat(srcPath)
		if err != nil {
			t.Fatal(err)
		}

		client := &fakeCheckContentClient{
			checkFunc: prescreenServer(false, branchdam.ContentCheckResult{}),
		}
		archiveRoot := filepath.Join(dir, "archive")
		localRoot := filepath.Join(dir, "local")
		e := newTestEngine(t, client, archiveRoot, localRoot)

		vars := TemplateVars{
			OriginalName: "DSC0006.JPG",
			CapturedAt:   srcInfo.ModTime(),
		}
		destArchive := filepath.Join(archiveRoot, RenderPath(e.Ingest.PathTemplate, vars))
		destLocal := filepath.Join(localRoot, RenderPath(e.Ingest.PathTemplate, vars))
		if err := os.MkdirAll(filepath.Dir(destArchive), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(destLocal), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destArchive, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destLocal, content, 0o644); err != nil {
			t.Fatal(err)
		}

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		// Copies at the destination prove nothing about registration:
		// with no queue row and the server not knowing the content, the
		// file is registered (from its existing copies), not skipped.
		fr := res.Files[0]
		if fr.Err != nil {
			t.Fatalf("unexpected file error: %v", fr.Err)
		}
		if fr.Skipped {
			t.Errorf("expected the unregistered copies to be registered, got Skipped (%q)", fr.SkipReason)
		}
		if len(client.checkCalls) != 1 || client.checkCalls[0].FullHash != blake3Hex(t, destLocal) {
			t.Errorf("CheckContent calls = %+v, want one with the copies' full hash", client.checkCalls)
		}
		if len(client.calls) != 1 || client.calls[0].FilePath != "/storage/archive/DSC0006.JPG" {
			t.Errorf("EVENT_NODE_CREATED posts = %+v, want one for /storage/archive/DSC0006.JPG", client.calls)
		}
	})

	t.Run("UploadStream mode bypasses pre-flight CheckContent", func(t *testing.T) {
		dir := t.TempDir()
		cardRoot := filepath.Join(dir, "card")
		if err := os.MkdirAll(cardRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cardRoot, "DSC0007.JPG"), []byte("upload-stream-data"), 0o644); err != nil {
			t.Fatal(err)
		}

		uploader := &fakeUploaderClient{}
		e := NewEngine(uploader, "test-agent", config.IngestConfig{
			LocalEditRoot:        filepath.Join(dir, "local"),
			UploadStream:         true,
			PreflightTimeoutSecs: 5,
		}, nil)

		res, err := e.IngestCard(context.Background(), cardRoot)
		if err != nil {
			t.Fatalf("IngestCard: %v", err)
		}
		if len(res.Files) != 1 {
			t.Fatalf("got %d files, want 1", len(res.Files))
		}
		if len(uploader.uploadCalls) != 1 {
			t.Errorf("expected 1 upload call, got %d", len(uploader.uploadCalls))
		}
	})
}

// TestIngestCardCameraModelTraversalE2E is the end-to-end assertion that
// closes the last open AC item from issue #99 / PR #128: a crafted
// camera_model of "../../evil" must not let RenderPath's output escape
// the archiveRoot when joined under it. TestRenderPathStripsTraversalSequence
// proves the RenderPath boundary in isolation; this test proves the same
// invariant holds after filepath.Join, which is the half the ingest
// pipeline actually exercises (every DualWrite destination is
// `filepath.Join(archiveRoot, relPath)`). The blast radius is identical:
// a traversal that survives Join would land a file above archiveRoot.
//
// Direct RenderPath invocation (not a full card driver) is sufficient
// because the join operation IS the security boundary -- there is no
// downstream check on the joined path -- and the existing test's
// table-driven cases already cover the path-rendering side.
func TestIngestCardCameraModelTraversalE2E(t *testing.T) {
	archiveRoot := t.TempDir()

	cases := []struct {
		name        string
		cameraModel string
	}{
		{"traversal_segments", "../../etc"},
		{"evil_literal", "../../evil"},
		{"bare_dot_segments", "./././"},
		{"mixed_traversal_and_literal", ".././../evil"},
	}

	tpl := "{camera_model}/{original_name}"
	now := time.Now()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := TemplateVars{
				CapturedAt:   now,
				CameraModel:  tc.cameraModel,
				OriginalName: "f.jpg",
			}
			rel := RenderPath(tpl, vars)
			joined := filepath.Join(archiveRoot, rel)

			// Blast-radius assertion: filepath.Rel must produce a
			// path that starts with ".." if the traversal survived
			// Join -- that, and only that, is the security
			// boundary. A relative rel would land the file above
			// archiveRoot. (filepath.IsAbs(joined) is always true
			// because t.TempDir() is absolute, so the test does
			// not assert on it; the Rel check above subsumes the
			// "joined is under archiveRoot" property.)
			relFromRoot, err := filepath.Rel(archiveRoot, joined)
			if err != nil {
				t.Fatalf("filepath.Rel(%q, %q): %v", archiveRoot, joined, err)
			}
			if relFromRoot == ".." || strings.HasPrefix(relFromRoot, ".."+string(filepath.Separator)) {
				t.Errorf("RenderPath(%q, %+v) = %q; joined = %q escapes archiveRoot (rel = %q)",
					tpl, vars, rel, joined, relFromRoot)
			}

			// Sanity check: the literal ".." must not appear in the
			// joined path, mirroring TestRenderPathStripsTraversalSequence.
			if strings.Contains(joined, "..") {
				t.Errorf("joined path %q still contains %q", joined, "..")
			}
		})
	}
}

func TestIngestCardUploadStreamPauseOnMetered(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0099.jpg"), []byte("metered-photo-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	localRoot := filepath.Join(dir, "local")

	// Case 1: pauseUploadOnMetered: true AND isMetered returns true
	// -> upload deferred, local copy proceeds, no Upload HTTP call
	uploader := &fakeUploaderClient{}
	e := NewEngine(uploader, "test-agent", config.IngestConfig{
		LocalEditRoot:        localRoot,
		UploadStream:         true,
		PauseUploadOnMetered: true,
	}, nil)
	e.IsMetered = func() (bool, error) { return true, nil }

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	fr := res.Files[0]
	if fr.Err != nil {
		t.Fatalf("unexpected error: %v", fr.Err)
	}
	if !fr.Skipped {
		t.Errorf("expected fr.Skipped=true, got false")
	}
	if !fr.Deferred {
		t.Errorf("expected fr.Deferred=true: the file never left the card, so the card is not done")
	}
	if fr.SkipReason != "upload skipped on metered connection -- re-insert card to retry" {
		t.Errorf("expected skipReason 'upload skipped on metered connection -- re-insert card to retry', got %q", fr.SkipReason)
	}
	if len(uploader.uploadCalls) != 0 {
		t.Errorf("expected 0 upload calls on metered network, got %d", len(uploader.uploadCalls))
	}
	if fr.LocalPath == "" {
		t.Errorf("expected local path to be populated, got empty")
	}
	if !fr.LocalVerify.Verified {
		t.Errorf("expected local copy to be verified, got verified=%v", fr.LocalVerify.Verified)
	}
	if _, err := os.Stat(fr.LocalPath); err != nil {
		t.Errorf("expected local file to exist at %s: %v", fr.LocalPath, err)
	}

	// Case 2: pauseUploadOnMetered: true AND isMetered returns false (unmetered connection)
	// -> upload proceeds normally
	uploader2 := &fakeUploaderClient{}
	e2 := NewEngine(uploader2, "test-agent", config.IngestConfig{
		LocalEditRoot:        filepath.Join(dir, "local2"),
		UploadStream:         true,
		PauseUploadOnMetered: true,
	}, nil)
	e2.IsMetered = func() (bool, error) { return false, nil }

	res2, err := e2.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res2.Files) != 1 || res2.Files[0].Err != nil {
		t.Fatalf("unexpected result: %+v", res2.Files)
	}
	if res2.Files[0].Skipped || res2.Files[0].Deferred {
		t.Errorf("expected fr.Skipped=false and Deferred=false on unmetered network, got %+v", res2.Files[0])
	}
	if len(uploader2.uploadCalls) != 1 {
		t.Errorf("expected 1 upload call on unmetered network, got %d", len(uploader2.uploadCalls))
	}

	// Case 3: pauseUploadOnMetered: false (default) AND isMetered returns true
	// -> no behavior change, upload proceeds
	uploader3 := &fakeUploaderClient{}
	e3 := NewEngine(uploader3, "test-agent", config.IngestConfig{
		LocalEditRoot:        filepath.Join(dir, "local3"),
		UploadStream:         true,
		PauseUploadOnMetered: false,
	}, nil)
	e3.IsMetered = func() (bool, error) { return true, nil }

	res3, err := e3.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res3.Files) != 1 || res3.Files[0].Err != nil {
		t.Fatalf("unexpected result: %+v", res3.Files)
	}
	if res3.Files[0].Skipped {
		t.Errorf("expected fr.Skipped=false when PauseUploadOnMetered is false, got true")
	}
	if len(uploader3.uploadCalls) != 1 {
		t.Errorf("expected 1 upload call when PauseUploadOnMetered is false, got %d", len(uploader3.uploadCalls))
	}
}

// TestDedupStateNoteErrorLatches pins which failures switch the content
// check off for the rest of a pass: the ones every later file would hit too.
func TestDedupStateNoteErrorLatches(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		latch bool
	}{
		{"transport error", errors.New("dial tcp: connection refused"), true},
		{"404 route missing", &branchdam.HTTPError{StatusCode: 404}, true},
		{"501 not implemented", &branchdam.HTTPError{StatusCode: 501}, true},
		{"401 invalid key", &branchdam.HTTPError{StatusCode: 401}, true},
		{"403 forbidden", &branchdam.HTTPError{StatusCode: 403}, true},
		{"429 rate limited", &branchdam.HTTPError{StatusCode: 429}, true},
		{"400 bad request", &branchdam.HTTPError{StatusCode: 400}, false},
		{"500 server error", &branchdam.HTTPError{StatusCode: 500}, false},
		{"503 unavailable", &branchdam.HTTPError{StatusCode: 503}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d dedupState
			d.noteError(tc.err)
			if d.unavailable != tc.latch {
				t.Errorf("unavailable = %v, want %v", d.unavailable, tc.latch)
			}
		})
	}
}

// TestCheckContentDedupWarnsOnTheLatchingFailure: the failure that turns
// dedup off for the pass is logged at Warn (visible at the default Info
// level); only the files after it, which skip the check, go quiet.
func TestCheckContentDedupWarnsOnTheLatchingFailure(t *testing.T) {
	buf := captureSlog(t)
	client := &fakeCheckContentClient{checkFunc: func(context.Context, branchdam.ContentCheckQuery) (branchdam.ContentCheckResult, error) {
		return branchdam.ContentCheckResult{}, &branchdam.HTTPError{StatusCode: 404, Body: "404 page not found"}
	}}
	e := newTestEngine(t, client, t.TempDir(), t.TempDir())
	dedup := &dedupState{}
	wr := WriteResult{FastHash: "0123456789abcdef", FullHash: strings.Repeat("a", 64), SizeBytes: 10}

	if skip, _, _ := e.checkContentDedup(context.Background(), "/card/A.JPG", wr, "", 5, dedup); skip {
		t.Fatal("a failed check must fail open")
	}
	if !dedup.unavailable {
		t.Fatal("a 404 must latch unavailable")
	}
	if !strings.Contains(buf.String(), `"level":"WARN"`) || !strings.Contains(buf.String(), "content check pre-flight failed") {
		t.Errorf("the latching failure was not logged at WARN:\n%s", buf.String())
	}
	if skip, _, _ := e.checkContentDedup(context.Background(), "/card/B.JPG", wr, "", 5, dedup); skip || len(client.checkCalls) != 1 {
		t.Errorf("after the latch: skip=%v, calls=%d, want no further request", skip, len(client.checkCalls))
	}
}
