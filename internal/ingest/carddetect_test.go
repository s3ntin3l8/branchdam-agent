package ingest

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestDetectorWatchReportsInsertedAndRemoved(t *testing.T) {
	polls := [][]string{
		{"/media/card1"},
		{"/media/card1", "/media/card2"},
		{"/media/card2"},
	}
	i := 0
	d := &Detector{
		Interval: time.Millisecond,
		List: func() ([]string, error) {
			cur := polls[i]
			if i < len(polls)-1 {
				i++
			}
			return cur, nil
		},
	}

	var diffs []Diff
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := d.Watch(ctx, func(diff Diff) {
		diffs = append(diffs, diff)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch err = %v, want context.Canceled", err)
	}

	if len(diffs) < 2 {
		t.Fatalf("got %d diffs, want at least 2 (initial insert, then card2 insert, then card1 removed)", len(diffs))
	}
	// First poll: card1 appears from nothing.
	sort.Strings(diffs[0].Inserted)
	if !reflect.DeepEqual(diffs[0].Inserted, []string{"/media/card1"}) {
		t.Errorf("diffs[0].Inserted = %v, want [/media/card1]", diffs[0].Inserted)
	}
	if len(diffs[0].Removed) != 0 {
		t.Errorf("diffs[0].Removed = %v, want none", diffs[0].Removed)
	}
}

func TestListVolumesUnderIgnoresMissingRoots(t *testing.T) {
	dir := t.TempDir()
	if err := mkdirs(dir, "media/card1", "media/card2"); err != nil {
		t.Fatal(err)
	}

	got, err := ListVolumesUnder([]string{dir + "/media", dir + "/nonexistent-root"})
	if err != nil {
		t.Fatalf("ListVolumesUnder: %v", err)
	}
	sort.Strings(got)
	want := []string{dir + "/media/card1", dir + "/media/card2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListVolumesUnder = %v, want %v", got, want)
	}
}

func mkdirs(base string, dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(base+"/"+d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
