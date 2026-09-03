package tray

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

func TestHandleImportFolderSuccess(t *testing.T) {
	fi := &fakeIngester{result: ingest.CardResult{
		Files: []ingest.FileResult{{SourcePath: "/media/external/folder/test.jpg"}},
	}}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	var notified bool
	notify := func(_ context.Context, _ string) {
		notified = true
	}
	pickDir := func(_ context.Context) (string, error) {
		return "/media/external/folder", nil
	}

	ingested, summary := handleImportFolder(context.Background(), r, pickDir, notify)
	if !ingested {
		t.Fatal("expected handleImportFolder to return ingested=true on success")
	}
	if notified {
		t.Error("expected notification not to be called on success")
	}
	if len(fi.calls) != 1 || fi.calls[0] != "/media/external/folder" {
		t.Errorf("expected IngestCard called once with /media/external/folder, got %v", fi.calls)
	}
	if summary.Submitted != 1 {
		t.Errorf("got submitted %d, want 1", summary.Submitted)
	}
}

func TestHandleImportFolderAlreadyWatched(t *testing.T) {
	fi := &fakeIngester{}
	watchDir := filepath.Join("media", "card")
	r := NewRunner(fi, []string{watchDir}, "/scratch")

	var gotMsg string
	notify := func(_ context.Context, msg string) {
		gotMsg = msg
	}
	// Test with trailing slash / redundant separator to ensure normalization
	pickDir := func(_ context.Context) (string, error) {
		return watchDir + string(filepath.Separator), nil
	}

	ingested, _ := handleImportFolder(context.Background(), r, pickDir, notify)
	if ingested {
		t.Fatal("expected handleImportFolder to return ingested=false when path is already watched")
	}
	if gotMsg != "Already ingesting this path" {
		t.Errorf("got notification message %q, want %q", gotMsg, "Already ingesting this path")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderCanceled(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	var notified bool
	notify := func(_ context.Context, _ string) {
		notified = true
	}
	pickDir := func(_ context.Context) (string, error) {
		return "", errors.New("canceled")
	}

	ingested, _ := handleImportFolder(context.Background(), r, pickDir, notify)
	if ingested {
		t.Fatal("expected handleImportFolder to return ingested=false when picker canceled")
	}
	if notified {
		t.Error("expected notification not to be called on picker cancel")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderEmptyPath(t *testing.T) {
	fi := &fakeIngester{}
	r := NewRunner(fi, []string{"/media/card"}, "/scratch")

	pickDir := func(_ context.Context) (string, error) {
		return "", nil
	}

	ingested, _ := handleImportFolder(context.Background(), r, pickDir, nil)
	if ingested {
		t.Fatal("expected handleImportFolder to return ingested=false on empty path")
	}
	if len(fi.calls) != 0 {
		t.Errorf("expected IngestCard not to be called, got calls=%v", fi.calls)
	}
}

func TestHandleImportFolderNilSafeguards(t *testing.T) {
	if ingested, _ := handleImportFolder(context.Background(), nil, nil, nil); ingested {
		t.Error("expected false for nil runner/pickDir")
	}
}
