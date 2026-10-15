package tray

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// mkVolume creates root/name (plus root/name/DCIM when dcim is true) and
// returns the volume path.
func mkVolume(t *testing.T, root, name string, dcim bool) string {
	t.Helper()
	vol := filepath.Join(root, name)
	dir := vol
	if dcim {
		dir = filepath.Join(vol, "DCIM")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return vol
}

// TestSelectCardVolumes pins "Ingest now"'s volume selection: only the
// volumes under cardRoots (never cardRoots itself), honouring requireDCIM,
// and never a volume that holds -- or sits inside -- the archive or local
// edit root, however that root is spelled.
func TestSelectCardVolumes(t *testing.T) {
	mounts := t.TempDir() // stands in for /Volumes or /media/$USER
	card := mkVolume(t, mounts, "EOS_DIGITAL", true)
	usb := mkVolume(t, mounts, "USB_STICK", false)
	nas := mkVolume(t, mounts, "NAS", true) // a NAS share with a DCIM folder of its own
	archiveRoot := filepath.Join(nas, "photos", "archive")
	if err := os.MkdirAll(archiveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	editSSD := mkVolume(t, mounts, "EDIT_SSD", false)
	localRoot := editSSD // localEditRoot is the volume root itself

	// archiveRoot configured through a symlink elsewhere (~/NAS -> /Volumes/NAS).
	linkDir := t.TempDir()
	archiveViaLink := filepath.Join(linkDir, "NAS")
	if err := os.Symlink(nas, archiveViaLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cases := []struct {
		name        string
		requireDCIM bool
		destRoots   []string
		want        []string
	}{
		{"no exclusions, no DCIM filter", false, nil, []string{card, editSSD, nas, usb}},
		{"requireDCIM", true, nil, []string{card, nas}},
		{"excludes volume containing archive root and the edit root volume", false, []string{archiveRoot, localRoot}, []string{card, usb}},
		{"requireDCIM and exclusions", true, []string{archiveRoot, localRoot}, []string{card}},
		{"archive root reached through a symlink", true, []string{filepath.Join(archiveViaLink, "photos", "archive")}, []string{card}},
		{"destination root that contains every volume", false, []string{mounts}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := selectCardVolumes([]string{mounts}, tc.requireDCIM, tc.destRoots...)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("selectCardVolumes = %v, want %v", got, want)
			}
			if slices.Contains(got, mounts) {
				t.Errorf("selectCardVolumes returned the cardRoots parent %s itself", mounts)
			}
		})
	}
}

// TestRunnerIngestNowVolumesExcludesDestinations is the Runner-level half:
// a WatchDirs parent that also holds the archive NAS and the edit SSD must
// yield only the card, with the Runner's own archiveRoot/localEditRoot and
// requireDCIM setting applied.
func TestRunnerIngestNowVolumesExcludesDestinations(t *testing.T) {
	mounts := t.TempDir()
	card := mkVolume(t, mounts, "CARD", true)
	mkVolume(t, mounts, "BACKUP", false)
	nas := mkVolume(t, mounts, "NAS", false)
	edit := mkVolume(t, mounts, "EDIT", false)

	r := NewRunner(&fakeIngester{}, []string{mounts}, filepath.Join(edit, "branchdam"))
	r.SetArchiveRoot(filepath.Join(nas, "archive"))

	got := r.IngestNowVolumes()
	slices.Sort(got)
	want := []string{filepath.Join(mounts, "BACKUP"), card}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("IngestNowVolumes() = %v, want %v (NAS and EDIT hold destinations)", got, want)
	}

	r.SetDetectorRequireDCIM(true)
	if got := r.IngestNowVolumes(); !slices.Equal(got, []string{card}) {
		t.Fatalf("IngestNowVolumes() with requireDCIM = %v, want [%s]", got, card)
	}
}

func TestIsCardVolumeRoot(t *testing.T) {
	roots := []string{"/Volumes", "/media/user/"}
	cases := []struct {
		path      string
		destRoots []string
		want      bool
	}{
		{"/Volumes/EOS_DIGITAL", nil, true},
		{"/Volumes/EOS_DIGITAL/", nil, true},
		{"/media/user/SONY", nil, true},
		{"/Volumes", nil, false},                                  // the parent itself
		{"/Volumes/EOS_DIGITAL/DCIM", nil, false},                 // a folder inside a card
		{"/Users/me/Pictures/import", nil, false},                 // an arbitrary folder
		{"/Volumes/NAS", []string{"/Volumes/NAS/archive"}, false}, // holds the archive
		{"/Volumes/EDIT", []string{"/Volumes/EDIT"}, false},       // is the edit root
		{"", nil, false},
	}
	for _, tc := range cases {
		if got := isCardVolumeRoot(tc.path, roots, tc.destRoots...); got != tc.want {
			t.Errorf("isCardVolumeRoot(%q, %v) = %v, want %v", tc.path, tc.destRoots, got, tc.want)
		}
	}
}

// TestShouldAutoEject is the table over every ingest source the tray has:
// only a detected, accepted card volume whose ingest fully succeeded may be
// ejected.
func TestShouldAutoEject(t *testing.T) {
	cardRoots := []string{"/Volumes"}
	destRoots := []string{"/Volumes/NAS/archive", "/Volumes/EDIT/branchdam"}
	ok := IngestSummary{Submitted: 3, Skipped: 1}
	cases := []struct {
		name        string
		autoEject   bool
		isDetection bool
		cardPath    string
		summary     IngestSummary
		want        bool
	}{
		{"detected card, clean ingest", true, true, "/Volumes/EOS_DIGITAL", ok, true},
		{"autoEject off", false, true, "/Volumes/EOS_DIGITAL", ok, false},
		{"Ingest now (manual) on a card volume", true, false, "/Volumes/EOS_DIGITAL", ok, false},
		{"Import from folder", true, false, "/Users/me/Pictures/import", ok, false},
		{"Import from folder on the edit SSD", true, false, "/Volumes/EDIT/stuff", ok, false},
		{"API ingest of the cardRoots parent", true, false, "/Volumes", ok, false},
		{"detection of a path that is not a volume root", true, true, "/Volumes/EOS_DIGITAL/DCIM", ok, false},
		{"detection of the NAS volume", true, true, "/Volumes/NAS", ok, false},
		{"detection of the edit SSD volume", true, true, "/Volumes/EDIT", ok, false},
		{"metered upload deferred", true, true, "/Volumes/EOS_DIGITAL", IngestSummary{Submitted: 2, Deferred: 1}, false},
		{"a file failed", true, true, "/Volumes/EOS_DIGITAL", IngestSummary{Submitted: 2, Failed: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoEject(tc.autoEject, tc.isDetection, tc.cardPath, cardRoots, destRoots, tc.summary); got != tc.want {
				t.Errorf("shouldAutoEject = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTriggerIngestEjectScope drives the real triggerIngest end to end:
// a manual ingest (Ingest now, Import from folder, /api/actions/ingest)
// never ejects, and a detected card whose upload was deferred on a metered
// connection is reported not-OK and stays mounted.
func TestTriggerIngestEjectScope(t *testing.T) {
	clean := ingest.CardResult{Files: []ingest.FileResult{{SourcePath: "/media/CARD/DCIM/IMG_0001.JPG"}}}
	deferred := ingest.CardResult{Files: []ingest.FileResult{
		{SourcePath: "/media/CARD/DCIM/IMG_0001.JPG"},
		{SourcePath: "/media/CARD/DCIM/IMG_0002.JPG", Skipped: true, Deferred: true, SkipReason: "upload skipped on metered connection -- re-insert card to retry"},
	}}
	cases := []struct {
		name       string
		result     ingest.CardResult
		detection  bool
		path       string
		wantEject  bool
		wantOK     bool
		wantDeferr int
	}{
		{"detected card", clean, true, "/media/CARD", true, true, 0},
		{"manual ingest of a card volume", clean, false, "/media/CARD", false, true, 0},
		{"manual folder import", clean, false, "/home/me/import", false, true, 0},
		{"detected card with metered deferral", deferred, true, "/media/CARD", false, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRunner(&fakeIngester{result: tc.result}, []string{"/media"}, "/scratch")
			r.SetAutoEject(true)
			var ejected []string
			r.SetEjectFunc(func(p string) error { ejected = append(ejected, p); return nil })
			var notes []string
			r.SetNotifier(func(_, msg string) { notes = append(notes, msg) })

			var sum IngestSummary
			if tc.detection {
				sum = r.TriggerDetectedIngest(context.Background(), tc.path)
			} else {
				sum = r.TriggerIngest(context.Background(), tc.path)
			}
			if sum.OK() != tc.wantOK {
				t.Errorf("OK() = %v, want %v (%+v)", sum.OK(), tc.wantOK, sum)
			}
			if sum.Deferred != tc.wantDeferr {
				t.Errorf("Deferred = %d, want %d", sum.Deferred, tc.wantDeferr)
			}
			if got := len(ejected) > 0; got != tc.wantEject {
				t.Errorf("ejected %v, want eject=%v", ejected, tc.wantEject)
			}
			if tc.wantDeferr > 0 && len(notes) == 0 {
				t.Error("expected a notification that the card is waiting for an unmetered connection")
			}
		})
	}
}
