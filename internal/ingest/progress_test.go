package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEngineProgressReportsCopyingAndVerifyingPhases confirms
// Engine.Progress -- wired via IngestCard's real ingestFile path, not a
// direct DualWrite/Verify call -- receives both phases in order, each
// reaching the file's full size, and that nil Progress (every other test
// in this package) changes nothing about the result.
func TestEngineProgressReportsCopyingAndVerifyingPhases(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 64*1024)
	if err := os.WriteFile(filepath.Join(cardRoot, "IMG_0001.jpg"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	var events []ProgressEvent
	e.Progress = func(ev ProgressEvent) { events = append(events, ev) }

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if res.Files[0].Err != nil {
		t.Fatalf("file error: %v", res.Files[0].Err)
	}

	if len(events) == 0 {
		t.Fatal("expected at least one progress event")
	}

	var sawCopying, sawVerifying bool
	for _, ev := range events {
		if ev.TotalBytes != int64(len(content)) {
			t.Errorf("event %+v: TotalBytes = %d, want %d", ev, ev.TotalBytes, len(content))
		}
		switch ev.Phase {
		case ProgressPhaseCopying:
			sawCopying = true
		case ProgressPhaseVerifying:
			sawVerifying = true
		default:
			t.Errorf("unexpected phase %q", ev.Phase)
		}
	}
	if !sawCopying {
		t.Error("expected at least one copying-phase event")
	}
	if !sawVerifying {
		t.Error("expected at least one verifying-phase event (both archive and local copies get verified)")
	}
	if first := events[0]; first.Phase != ProgressPhaseCopying {
		t.Errorf("first event's phase = %q, want copying (DualWrite runs before either Verify)", first.Phase)
	}
}

// TestDualWriteReportsProgress confirms WithProgress's cumulative byte
// count reaches exactly the source size and never decreases -- the
// contract a live "N of M bytes" readout depends on.
func TestDualWriteReportsProgress(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 3*1024*1024)
	archivePath := filepath.Join(dir, "archive", "photo.bin")
	localPath := filepath.Join(dir, "local", "photo.bin")

	var samples []int64
	_, err := DualWrite(src, archivePath, localPath, WithProgress(func(n int64) {
		samples = append(samples, n)
	}))
	if err != nil {
		t.Fatalf("DualWrite: %v", err)
	}
	assertMonotonicProgress(t, samples, 3*1024*1024)
}

func TestWriteLocalReportsProgress(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 2*1024*1024)
	localPath := filepath.Join(dir, "local", "photo.bin")

	var samples []int64
	_, err := WriteLocal(src, localPath, WithProgress(func(n int64) {
		samples = append(samples, n)
	}))
	if err != nil {
		t.Fatalf("WriteLocal: %v", err)
	}
	assertMonotonicProgress(t, samples, 2*1024*1024)
}

func TestVerifyReportsProgress(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 2*1024*1024)
	localPath := filepath.Join(dir, "local", "photo.bin")

	writeRes, err := WriteLocal(src, localPath)
	if err != nil {
		t.Fatal(err)
	}

	var samples []int64
	vr, err := Verify(localPath, writeRes.FullHash, WithProgress(func(n int64) {
		samples = append(samples, n)
	}))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.Verified {
		t.Fatal("expected Verify to succeed")
	}
	assertMonotonicProgress(t, samples, 2*1024*1024)
}

func TestCopyToArchiveReportsProgress(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, 2*1024*1024)
	localPath := filepath.Join(dir, "local", "photo.bin")
	archivePath := filepath.Join(dir, "archive", "photo.bin")

	writeRes, err := WriteLocal(src, localPath)
	if err != nil {
		t.Fatal(err)
	}

	var samples []int64
	err = CopyToArchive(localPath, archivePath, writeRes.FullHash, WithProgress(func(n int64) {
		samples = append(samples, n)
	}))
	if err != nil {
		t.Fatalf("CopyToArchive: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("expected at least one progress sample")
	}
	// CopyToArchive's opts thread through both its own copy AND its
	// pre/post Verify calls (three separate byte-count sequences over the
	// same file, each its own pass) -- so unlike the single-pass cases
	// above, only the final sample of the LAST pass is guaranteed to
	// reach the full size, and the overall sequence need not be
	// monotonic across pass boundaries. Assert the weaker, still
	// meaningful property: every individual sample is in [1, size], and
	// the last one recorded reaches size (CopyToArchive's own final
	// Verify is always the last pass to run).
	for _, s := range samples {
		if s < 1 || s > 2*1024*1024 {
			t.Fatalf("sample %d out of range [1, %d]", s, 2*1024*1024)
		}
	}
	if last := samples[len(samples)-1]; last != 2*1024*1024 {
		t.Errorf("final sample = %d, want %d (the closing Verify pass)", last, 2*1024*1024)
	}
}

// TestDrainReportsProgressDuringArchiveCopy confirms WithDrainProgress
// reaches a real callback during Drain's phase 2 archive copy -- the
// plumbing PR6's tray timer will actually consume.
func TestDrainReportsProgressDuringArchiveCopy(t *testing.T) {
	dir := t.TempDir()
	store := openStoreT(t)
	content := make([]byte, 1024*1024)
	seedMediaRow(t, store, dir, "uuid-progress", content)
	archivePath := filepath.Join(dir, "archive", "uuid-progress.bin")

	var events []ProgressEvent
	client := &fakeDrainClient{}
	_, err := Drain(context.Background(), client, store, "agent-1", fixedNow(time.Unix(1_800_000_000, 0)),
		WithDrainProgress(func(ev ProgressEvent) {
			events = append(events, ev)
		}))
	if err != nil {
		t.Fatal(err)
	}

	if len(events) == 0 {
		t.Fatal("expected at least one progress event during Drain's archive copy")
	}
	for _, ev := range events {
		if ev.Path != archivePath {
			t.Errorf("event.Path = %q, want %q", ev.Path, archivePath)
		}
		if ev.Phase != ProgressPhaseCopying {
			t.Errorf("event.Phase = %q, want %q", ev.Phase, ProgressPhaseCopying)
		}
		if ev.TotalBytes != int64(len(content)) {
			t.Errorf("event.TotalBytes = %d, want %d", ev.TotalBytes, len(content))
		}
	}
	if last := events[len(events)-1].BytesDone; last != int64(len(content)) {
		t.Errorf("final event.BytesDone = %d, want %d", last, len(content))
	}
}

// assertMonotonicProgress is the shared shape for the single-pass cases
// (DualWrite, WriteLocal, Verify): each is exactly one copy loop over the
// file, so progress must be strictly increasing and its last sample must
// equal the file's full size.
func assertMonotonicProgress(t *testing.T, samples []int64, wantFinal int64) {
	t.Helper()
	if len(samples) == 0 {
		t.Fatal("expected at least one progress sample")
	}
	prev := int64(0)
	for i, s := range samples {
		if s <= prev {
			t.Errorf("sample %d (%d) did not increase over previous (%d)", i, s, prev)
		}
		prev = s
	}
	if got := samples[len(samples)-1]; got != wantFinal {
		t.Errorf("final sample = %d, want %d", got, wantFinal)
	}
}
