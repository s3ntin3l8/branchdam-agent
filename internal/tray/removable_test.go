package tray

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// fakeRemovableDrives scripts a Windows-style removable-drive layout on any
// OS: removableDriveDetection on, listCardVolumes adding the current
// drives to the cardRoots volumes, and isRemovableDriveRoot recognising
// exactly those drive roots.
type fakeRemovableDrives struct {
	mu     sync.Mutex
	drives []string
}

func (f *fakeRemovableDrives) set(drives ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drives = drives
}

func (f *fakeRemovableDrives) current() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.drives...)
}

func installFakeRemovableDrives(t *testing.T) *fakeRemovableDrives {
	t.Helper()
	f := &fakeRemovableDrives{}
	origList, origIs, origDetect := listCardVolumes, isRemovableDriveRoot, removableDriveDetection
	removableDriveDetection = true
	listCardVolumes = func(cardRoots []string, requireDCIM bool) ([]string, error) {
		vols, err := ingest.ListVolumesUnder(cardRoots, requireDCIM)
		return append(vols, f.current()...), err
	}
	isRemovableDriveRoot = func(p string) bool { return slices.Contains(f.current(), p) }
	t.Cleanup(func() { listCardVolumes, isRemovableDriveRoot, removableDriveDetection = origList, origIs, origDetect })
	return f
}

// withoutRemovableDetection pins the cardRoots-only behaviour (Linux and
// macOS) for a test, so it holds on a Windows test runner too.
func withoutRemovableDetection(t *testing.T) {
	t.Helper()
	orig := removableDriveDetection
	removableDriveDetection = false
	t.Cleanup(func() { removableDriveDetection = orig })
}

// TestDetectorWatchesRemovableDrivesWithoutCardRoots: on Windows there is
// no cardRoots parent to configure, so an empty roots list must still run
// the detector, and a removable drive appearing must go through the same
// TriggerDetectedIngest path a /Volumes subdirectory does elsewhere.
func TestDetectorWatchesRemovableDrivesWithoutCardRoots(t *testing.T) {
	drives := installFakeRemovableDrives(t)
	fi := &fakeIngester{}
	r := NewRunner(fi, nil, "")
	r.SetDetectorInterval(5 * time.Millisecond)
	var hits int32
	r.SetOnCardIngested(func() { atomic.AddInt32(&hits, 1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ReconfigureDetector(ctx, nil)

	r.detectorMu.Lock()
	running := r.detectorCancel != nil
	r.detectorMu.Unlock()
	if !running {
		t.Fatal("detector not started with empty cardRoots, but removable-drive detection needs none")
	}

	card := t.TempDir() // stands in for `E:\`
	drives.set(card)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&hits) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.StopDetector()
	if len(fi.calls) != 1 || fi.calls[0] != card {
		t.Errorf("ingested %v, want exactly the inserted removable drive %s", fi.calls, card)
	}
}

// TestRemovableDriveRootIsCardVolumeRoot: a detected removable drive root
// is a card volume root for auto-eject (it has no cardRoots parent), but a
// folder on it is not, and a removable drive holding the archive or local
// edit root never is.
func TestRemovableDriveRootIsCardVolumeRoot(t *testing.T) {
	drives := installFakeRemovableDrives(t)
	card := t.TempDir()
	drives.set(card)
	archive := t.TempDir()

	if !isCardVolumeRoot(card, nil, archive) {
		t.Errorf("isCardVolumeRoot(%s) = false for a removable drive root", card)
	}
	if isCardVolumeRoot(filepath.Join(card, "DCIM"), nil, archive) {
		t.Error("a folder on a removable drive counted as a card volume root")
	}
	if isCardVolumeRoot(card, nil, filepath.Join(card, "Archive")) {
		t.Error("a removable drive holding the archive root counted as a card volume root")
	}
	ok := IngestSummary{CardPath: card, Submitted: 1}
	if !shouldAutoEject(true, true, card, nil, []string{archive}, ok) {
		t.Error("shouldAutoEject = false for a verified detection ingest of a removable drive root")
	}
	if shouldAutoEject(true, false, card, nil, []string{archive}, ok) {
		t.Error("shouldAutoEject = true for a manual ingest")
	}
}

func TestIngestNowEmptyMessageWithRemovableDetection(t *testing.T) {
	installFakeRemovableDrives(t)
	if got := ingestNowEmptyMessage(nil); got != "No camera card found to ingest" {
		t.Errorf("ingestNowEmptyMessage(nil) = %q; with removable-drive detection no cardRoots is normal, not a setup gap", got)
	}
}

func TestIngestNowVolumesIncludesRemovableDrives(t *testing.T) {
	drives := installFakeRemovableDrives(t)
	card := t.TempDir()
	if err := os.MkdirAll(filepath.Join(card, "DCIM"), 0o755); err != nil {
		t.Fatal(err)
	}
	drives.set(card)
	r := NewRunner(&fakeIngester{}, nil, "")
	if got := r.IngestNowVolumes(); len(got) != 1 || got[0] != card {
		t.Errorf("IngestNowVolumes = %v, want [%s]", got, card)
	}
}

func TestVolumeName(t *testing.T) {
	for in, want := range map[string]string{
		"/Volumes/EOS_DIGITAL": "EOS_DIGITAL",
		"/":                    "/",
	} {
		if got := volumeName(in); got != want {
			t.Errorf("volumeName(%q) = %q, want %q", in, got, want)
		}
	}
}
