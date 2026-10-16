package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Drive types GetDriveType reports, for the fakes below.
const (
	driveFixedT     = 3
	driveRemoteT    = 4
	driveCDROMT     = 5
	driveRemovableT = driveRemovable
)

func maskOf(letters ...byte) uint32 {
	var m uint32
	for _, l := range letters {
		m |= 1 << uint(l-'A')
	}
	return m
}

func fakeDriveTypes(types map[string]uint32) func(string) uint32 {
	return func(root string) uint32 { return types[root] }
}

func TestRemovableDriveRoots(t *testing.T) {
	types := fakeDriveTypes(map[string]uint32{
		`A:\`: driveRemovableT, // legacy floppy letter: never probed
		`C:\`: driveFixedT,     // system disk
		`D:\`: driveRemovableT, // SD card
		`E:\`: driveRemovableT, // empty card-reader slot
		`F:\`: driveRemoteT,    // mapped NAS share
		`G:\`: driveCDROMT,
		`H:\`: driveFixedT, // USB SSD (edit disk)
		`Z:\`: driveRemovableT,
	})
	present := func(root string) bool { return root != `E:\` }
	got := removableDriveRoots(maskOf('A', 'C', 'D', 'E', 'F', 'G', 'H', 'Z'), types, present)
	if want := []string{`D:\`, `Z:\`}; !reflect.DeepEqual(got, want) {
		t.Errorf("removableDriveRoots = %v, want %v", got, want)
	}
	if got := removableDriveRoots(maskOf('D'), types, nil); !reflect.DeepEqual(got, []string{`D:\`}) {
		t.Errorf("nil present func = %v, want [D:\\]", got)
	}
	if got := removableDriveRoots(0, types, present); got != nil {
		t.Errorf("empty mask = %v, want nil", got)
	}
}

func TestDriveRoot(t *testing.T) {
	for in, want := range map[string]string{
		`E:\`:        `E:\`,
		`e:`:         `E:\`,
		`e:/`:        `E:\`,
		`E:\DCIM`:    "",
		`\\.\E:`:     "",
		`/Volumes/E`: "",
		`1:\`:        "",
		``:           "",
	} {
		if got := driveRoot(in); got != want {
			t.Errorf("driveRoot(%q) = %q, want %q", in, got, want)
		}
	}
}

func withDriveHooks(t *testing.T, list func() ([]string, error), types map[string]uint32) {
	t.Helper()
	origList, origType := listRemovableDrivesFn, driveTypeFn
	listRemovableDrivesFn = list
	driveTypeFn = fakeDriveTypes(types)
	t.Cleanup(func() { listRemovableDrivesFn, driveTypeFn = origList, origType })
}

func TestIsRemovableDriveRoot(t *testing.T) {
	withDriveHooks(t, nil, map[string]uint32{`E:\`: driveRemovableT, `D:\`: driveFixedT})
	for in, want := range map[string]bool{
		`E:\`:      true,
		`e:`:       true,
		`E:\DCIM`:  false, // a folder on the card is not its root
		`D:\`:      false, // fixed disk
		`F:\`:      false, // no such drive
		`/media/x`: false,
	} {
		if got := IsRemovableDriveRoot(in); got != want {
			t.Errorf("IsRemovableDriveRoot(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestListCardVolumesMergesRemovableDrives: removable drives join the
// cardRoots volumes, deduplicated, filtered by requireDCIM, and a failed
// drive listing degrades to the cardRoots volumes instead of an error
// (which would end the detector's Watch loop).
func TestListCardVolumesMergesRemovableDrives(t *testing.T) {
	parent := t.TempDir()
	cardUnderRoot := filepath.Join(parent, "CARD")
	driveWithDCIM := t.TempDir()
	driveWithoutDCIM := t.TempDir()
	for _, d := range []string{filepath.Join(cardUnderRoot, "DCIM"), filepath.Join(driveWithDCIM, "DCIM")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	drives := []string{driveWithDCIM, driveWithoutDCIM, cardUnderRoot}
	withDriveHooks(t, func() ([]string, error) { return drives, nil }, nil)

	got, err := ListCardVolumes([]string{parent}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{cardUnderRoot, driveWithDCIM, driveWithoutDCIM}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListCardVolumes = %v, want %v (no duplicate of the cardRoots volume)", got, want)
	}
	got, _ = ListCardVolumes([]string{parent}, true)
	if want := []string{cardUnderRoot, driveWithDCIM}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListCardVolumes(requireDCIM) = %v, want %v", got, want)
	}

	listRemovableDrivesFn = func() ([]string, error) { return nil, errors.New("GetLogicalDrives failed") }
	got, err = ListCardVolumes([]string{parent}, false)
	if err != nil || !reflect.DeepEqual(got, []string{cardUnderRoot}) {
		t.Errorf("drive listing failure: ListCardVolumes = %v, %v; want the cardRoots volume and no error", got, err)
	}
}

// TestDetectorDiffsRemovableDrives drives the real Detector.Watch loop
// over a scripted sequence of logical-drive bitmasks -- a card inserted
// into D:, a second into E:, D: pulled -- and checks the inserted/removed
// diffs it reports, the same diffs the tray's detection path acts on.
func TestDetectorDiffsRemovableDrives(t *testing.T) {
	types := fakeDriveTypes(map[string]uint32{`C:\`: driveFixedT, `D:\`: driveRemovableT, `E:\`: driveRemovableT})
	masks := []uint32{
		maskOf('C'),
		maskOf('C', 'D'),
		maskOf('C', 'D'),
		maskOf('C', 'D', 'E'),
		maskOf('C', 'E'),
	}
	var mu sync.Mutex
	poll := 0
	d := &Detector{
		Interval: time.Millisecond,
		List: func() ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			m := masks[len(masks)-1]
			if poll < len(masks) {
				m = masks[poll]
			}
			poll++
			return removableDriveRoots(m, types, nil), nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var diffs []Diff
	err := d.Watch(ctx, func(diff Diff) {
		diffs = append(diffs, diff)
		if len(diffs) == 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch returned %v, want context.Canceled after 3 diffs", err)
	}
	want := []Diff{
		{Inserted: []string{`D:\`}},
		{Inserted: []string{`E:\`}},
		{Removed: []string{`D:\`}},
	}
	if !reflect.DeepEqual(diffs, want) {
		t.Errorf("diffs = %+v, want %+v", diffs, want)
	}
}
