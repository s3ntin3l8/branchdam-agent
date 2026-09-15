package resolve

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCandidateDatabaseURLsWindows(t *testing.T) {
	appData := t.TempDir()
	urls := CandidateDatabaseURLs("windows", "", appData)
	if len(urls) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(urls))
	}
	want := "file:" + filepath.Join(appData, "Blackmagic Design", "DaVinci Resolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db") + "?mode=ro"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestCandidateDatabaseURLsWindowsNoLocalAppData(t *testing.T) {
	urls := CandidateDatabaseURLs("windows", "", "")
	if urls != nil {
		t.Errorf("expected nil for empty LOCALAPPDATA, got %v", urls)
	}
}

func TestCandidateDatabaseURLsDarwin(t *testing.T) {
	home := "/Users/alice"
	urls := CandidateDatabaseURLs("darwin", home, "")
	if len(urls) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(urls))
	}
	want := "file:" + filepath.Join(home, "Library", "Application Support", "Blackmagic Design", "DaVinci Resolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db") + "?mode=ro"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestCandidateDatabaseURLsDarwinEmptyHome(t *testing.T) {
	urls := CandidateDatabaseURLs("darwin", "", "")
	if urls != nil {
		t.Errorf("expected nil for empty home, got %v", urls)
	}
}

func TestCandidateDatabaseURLsLinux(t *testing.T) {
	home := "/home/bob"
	urls := CandidateDatabaseURLs("linux", home, "")
	if len(urls) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(urls))
	}
	want := "file:" + filepath.Join(home, ".local", "share", "DaVinciResolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db") + "?mode=ro"
	if urls[0] != want {
		t.Errorf("got %q, want %q", urls[0], want)
	}
}

func TestCandidateDatabaseURLsLinuxEmptyHome(t *testing.T) {
	urls := CandidateDatabaseURLs("linux", "", "")
	if urls != nil {
		t.Errorf("expected nil for empty home, got %v", urls)
	}
}

func TestCandidateDatabaseURLsUnsupported(t *testing.T) {
	urls := CandidateDatabaseURLs("plan9", "/usr/local", "")
	// plan9 falls into the default branch — should return a Linux-style path.
	if len(urls) != 1 {
		t.Fatalf("expected 1 candidate for plan9 (default branch), got %d", len(urls))
	}
}

func TestDiscoverDatabaseURLFound(t *testing.T) {
	// Create a temp SQLite database.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := openTestDB(dbPath)
	if err != nil {
		t.Fatalf("create test db: %v", err)
	}
	_ = db.Close()

	candidates := []string{"file:" + dbPath + "?mode=ro"}
	got, err := DiscoverDatabaseURL(context.Background(), candidates)
	if err != nil {
		t.Fatalf("DiscoverDatabaseURL: %v", err)
	}
	if got != candidates[0] {
		t.Errorf("got %q, want %q", got, candidates[0])
	}
}

func TestDiscoverDatabaseURLNotFound(t *testing.T) {
	candidates := []string{"file:" + filepath.Join(t.TempDir(), "nonexistent.db") + "?mode=ro"}
	got, err := DiscoverDatabaseURL(context.Background(), candidates)
	if err != nil {
		t.Fatalf("DiscoverDatabaseURL: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for nonexistent db, got %q", got)
	}
}

func TestDiscoverDatabaseURLBroken(t *testing.T) {
	// Create a file that's not a valid SQLite database.
	tmpDir := t.TempDir()
	badPath := filepath.Join(tmpDir, "bad.db")
	if err := os.WriteFile(badPath, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("write bad db: %v", err)
	}
	candidates := []string{"file:" + badPath + "?mode=ro"}
	got, err := DiscoverDatabaseURL(context.Background(), candidates)
	if err != nil {
		t.Fatalf("DiscoverDatabaseURL: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string for corrupt db, got %q", got)
	}
}

func TestDiscoverDatabaseURLSkipsCorruptCandidate(t *testing.T) {
	// First candidate is bad, second is good.
	tmpDir := t.TempDir()
	badPath := filepath.Join(tmpDir, "bad.db")
	if err := os.WriteFile(badPath, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("write bad db: %v", err)
	}
	goodPath := filepath.Join(tmpDir, "good.db")
	db, err := openTestDB(goodPath)
	if err != nil {
		t.Fatalf("create good db: %v", err)
	}
	_ = db.Close()

	candidates := []string{
		"file:" + badPath + "?mode=ro",
		"file:" + goodPath + "?mode=ro",
	}
	got, err := DiscoverDatabaseURL(context.Background(), candidates)
	if err != nil {
		t.Fatalf("DiscoverDatabaseURL: %v", err)
	}
	if got != candidates[1] {
		t.Errorf("got %q, want %q (should skip bad and find good)", got, candidates[1])
	}
}

func TestDiscoverDatabaseURLForGOOS(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows — LOCALAPPDATA not set in test env")
	}
	// On the current host's GOOS with a nonexistent home, should return "".
	got, err := DiscoverDatabaseURLForGOOS(context.Background(), runtime.GOOS, "", "")
	if err != nil {
		t.Fatalf("DiscoverDatabaseURLForGOOS: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty for empty home, got %q", got)
	}
}

// openTestDB creates a minimal SQLite database at path for testing.
// It uses database/sql directly to create the file, then returns a
// read-only *DB handle via Open.
func openTestDB(path string) (*DB, error) {
	// Create the database file with a table so it's a valid SQLite DB.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE IF NOT EXISTS _probe (id INTEGER PRIMARY KEY)"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return Open(context.Background(), "file:"+path+"?mode=ro")
}
