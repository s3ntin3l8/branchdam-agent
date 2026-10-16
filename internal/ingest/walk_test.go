package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestShouldSkipDir pins which directories the card walk refuses to
// descend. root itself is never skipped, even with a dot name.
func TestShouldSkipDir(t *testing.T) {
	root := filepath.Join("mnt", "CARD")
	cases := []struct {
		name string
		path string
		skip bool
	}{
		{"macOS trash", ".Trashes", true},
		{"Spotlight index", ".Spotlight-V100", true},
		{"FSEvents log", ".fseventsd", true},
		{"temporary items", ".TemporaryItems", true},
		{"any dot-directory", ".hidden", true},
		{"Windows volume metadata", "System Volume Information", true},
		{"Windows volume metadata lowercase", "system volume information", true},
		{"Windows recycle bin", "$RECYCLE.BIN", true},
		{"Windows recycle bin lowercase", "$Recycle.Bin", true},
		{"DCIM", "DCIM", false},
		{"camera folder", filepath.Join("DCIM", "100MSDCF"), false},
		{"nested dot-directory", filepath.Join("DCIM", ".thumbnails"), true},
		{"PRIVATE", "PRIVATE", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := shouldSkipDir(root, filepath.Join(root, c.path))
			if got != c.skip {
				t.Errorf("shouldSkipDir(%q) = %v, want %v", c.path, got, c.skip)
			}
			if got && !strings.HasPrefix(reason, "OS metadata directory:") && !strings.HasPrefix(reason, "hidden dot-directory:") {
				t.Errorf("reason = %q, want an \"OS metadata directory:\" or \"hidden dot-directory:\" reason", reason)
			}
		})
	}
	for _, r := range []string{root, ".card", filepath.Join("Volumes", ".hidden-card")} {
		if got, _ := shouldSkipDir(r, r); got {
			t.Errorf("shouldSkipDir(root=%q) = true, want the walk root never skipped", r)
		}
	}
}

// writeMetadataDirCard builds a card whose only real photo sits next to
// OS-maintained directories holding files whose own basenames pass
// shouldSkipByName -- the files the walk used to ingest.
func writeMetadataDirCard(t *testing.T, cardRoot string) {
	t.Helper()
	for rel, content := range map[string]string{
		filepath.Join("DCIM", "100MSDCF", "DSC0001.JPG"):                    "photo",
		filepath.Join(".Trashes", "501", "x.jpg"):                           "deleted photo",
		filepath.Join(".Spotlight-V100", "Store-V2", "store.db"):            "index",
		filepath.Join(".fseventsd", "0000000000000001"):                     "log",
		filepath.Join("System Volume Information", "WPSettings.dat"):        "settings",
		filepath.Join("System Volume Information", "IndexerVolumeGuid"):     "guid",
		filepath.Join("$RECYCLE.BIN", "S-1-5-21", "$R1234.JPG"):             "recycled photo",
		filepath.Join("DCIM", "100MSDCF", ".thumbnails", "DSC0001_thm.jpg"): "thumb",
	} {
		p := filepath.Join(cardRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// metadataDirs are the directories writeMetadataDirCard creates that the
// walk must report as skipped (and not descend).
var metadataDirs = []string{".Trashes", ".Spotlight-V100", ".fseventsd", "System Volume Information", "$RECYCLE.BIN", ".thumbnails"}

func assertNothingFromMetadataDirs(t *testing.T, destRoots ...string) {
	t.Helper()
	for _, root := range destRoots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Base(path) != "DSC0001.JPG" {
				t.Errorf("file from an OS metadata directory reached a destination: %s", path)
			}
			return nil
		})
	}
}

func TestIngestCardSkipsOSMetadataDirectories(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeMetadataDirCard(t, cardRoot)

	client := &fakeClient{}
	archiveRoot, localRoot := filepath.Join(dir, "archive"), filepath.Join(dir, "local")
	e := newTestEngine(t, client, archiveRoot, localRoot)

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(client.calls) != 1 {
		t.Errorf("PostNodeCreated calls = %d, want 1 (only DCIM/100MSDCF/DSC0001.JPG)", len(client.calls))
	}
	var skippedDirs []string
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Errorf("unexpected error for %s: %v", fr.SourcePath, fr.Err)
		}
		if fr.Skipped {
			skippedDirs = append(skippedDirs, filepath.Base(fr.SourcePath))
			continue
		}
		if filepath.Base(fr.SourcePath) != "DSC0001.JPG" {
			t.Errorf("ingested %s, want only DSC0001.JPG", fr.SourcePath)
		}
	}
	if !sameSet(skippedDirs, metadataDirs) {
		t.Errorf("skipped entries = %v, want exactly the metadata directories %v", skippedDirs, metadataDirs)
	}
	assertNothingFromMetadataDirs(t, archiveRoot, localRoot)
}

func TestIngestCardOfflineSkipsOSMetadataDirectories(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeMetadataDirCard(t, cardRoot)

	store := openStoreT(t)
	localRoot := filepath.Join(dir, "local")
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), localRoot, "/storage/staging/agent-1", store)

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v", err)
	}
	var skippedDirs []string
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Errorf("unexpected error for %s: %v", fr.SourcePath, fr.Err)
		}
		if fr.Skipped {
			skippedDirs = append(skippedDirs, filepath.Base(fr.SourcePath))
		}
	}
	if !sameSet(skippedDirs, metadataDirs) {
		t.Errorf("skipped entries = %v, want exactly the metadata directories %v", skippedDirs, metadataDirs)
	}
	all, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || filepath.Base(all[0].SourcePath) != "DSC0001.JPG" {
		t.Errorf("queue rows = %d (%v), want only DSC0001.JPG", len(all), all)
	}
	assertNothingFromMetadataDirs(t, localRoot)
}

// writeUnreadableDirCard builds a card with readable photos on both sides
// (in walk order) of a subdirectory the process cannot read, and returns
// that directory's path. Skips the test where chmod cannot make a
// directory unreadable: Windows, and root (which ignores the mode bits).
func writeUnreadableDirCard(t *testing.T, cardRoot string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not make a directory unreadable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a chmod 000 directory regardless")
	}
	for _, rel := range []string{
		filepath.Join("A", "IMG_0001.jpg"),
		filepath.Join("B", "IMG_0002.jpg"),
		filepath.Join("C", "IMG_0003.jpg"),
	} {
		p := filepath.Join(cardRoot, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("photo "+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locked := filepath.Join(cardRoot, "B")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	return locked
}

// TestIngestCardUnreadableSubdirKeepsPartialResult: one unreadable
// directory used to make IngestCard return CardResult{} plus an error,
// discarding the results of files it had already copied and submitted and
// never reaching the rest of the card.
func TestIngestCardUnreadableSubdirKeepsPartialResult(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	locked := writeUnreadableDirCard(t, cardRoot)

	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res, err := e.IngestCard(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v, want the partial result with no error", err)
	}
	var ok, failed []string
	for _, fr := range res.Files {
		if fr.Err != nil {
			failed = append(failed, fr.SourcePath)
			continue
		}
		ok = append(ok, filepath.Base(fr.SourcePath))
	}
	if !sameSet(ok, []string{"IMG_0001.jpg", "IMG_0003.jpg"}) {
		t.Errorf("ingested %v, want IMG_0001.jpg and IMG_0003.jpg", ok)
	}
	if len(failed) != 1 || failed[0] != locked {
		t.Errorf("failed entries = %v, want exactly %s", failed, locked)
	}
	if len(client.calls) != 2 {
		t.Errorf("PostNodeCreated calls = %d, want 2", len(client.calls))
	}
}

func TestIngestCardOfflineUnreadableSubdirKeepsPartialResult(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	locked := writeUnreadableDirCard(t, cardRoot)

	store := openStoreT(t)
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
	n := 0
	e.NewNodeUUID = func() (string, error) { n++; return fmt.Sprintf("uuid-%d", n), nil }

	res, err := e.IngestCardOffline(context.Background(), cardRoot)
	if err != nil {
		t.Fatalf("IngestCardOffline: %v, want the partial result with no error", err)
	}
	var queued, failed []string
	for _, fr := range res.Files {
		if fr.Err != nil {
			failed = append(failed, fr.SourcePath)
			continue
		}
		if fr.Queued {
			queued = append(queued, filepath.Base(fr.SourcePath))
		}
	}
	if !sameSet(queued, []string{"IMG_0001.jpg", "IMG_0003.jpg"}) {
		t.Errorf("queued %v, want IMG_0001.jpg and IMG_0003.jpg", queued)
	}
	if len(failed) != 1 || failed[0] != locked {
		t.Errorf("failed entries = %v, want exactly %s", failed, locked)
	}
}

// TestIngestCardMissingRootIsAnError: with no card to walk there is no
// partial result to keep -- that still surfaces as an error.
func TestIngestCardMissingRootIsAnError(t *testing.T) {
	dir := t.TempDir()
	e := newTestEngine(t, &fakeClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	if _, err := e.IngestCard(context.Background(), filepath.Join(dir, "no-such-card")); err == nil {
		t.Error("IngestCard on a missing root: err = nil, want an error")
	}
	store := openStoreT(t)
	eo := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive"), filepath.Join(dir, "local"), "/storage/staging/agent-1", store)
	if _, err := eo.IngestCardOffline(context.Background(), filepath.Join(dir, "no-such-card")); err == nil {
		t.Error("IngestCardOffline on a missing root: err = nil, want an error")
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, w := range want {
		seen[w]++
	}
	for _, g := range got {
		if seen[g] == 0 {
			return false
		}
		seen[g]--
	}
	return true
}
