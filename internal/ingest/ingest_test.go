package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
