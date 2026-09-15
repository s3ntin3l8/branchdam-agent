package resolve

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// CandidateDatabaseURLs returns file: URLs for DaVinci Resolve's local
// SQLite project database for this OS. home/appData are injected
// (rather than read from os.UserHomeDir/os.Getenv here) so this is
// table-testable on any host, including this repo's own Linux dev/CI
// machine, without touching the real filesystem or environment.
//
// The returned URLs use the file: URI scheme with ?mode=ro, matching
// the Open convention in db.go. Each candidate is a path where DaVinci
// Resolve 19 stores its local project cache database on the given
// platform.
func CandidateDatabaseURLs(goos, home, appData string) []string {
	switch goos {
	case "windows":
		if appData == "" {
			return nil
		}
		dbPath := filepath.Join(appData, "Blackmagic Design", "DaVinci Resolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db")
		return []string{"file:" + dbPath + "?mode=ro"}
	case "darwin":
		if home == "" {
			return nil
		}
		dbPath := filepath.Join(home, "Library", "Application Support", "Blackmagic Design", "DaVinci Resolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db")
		return []string{"file:" + dbPath + "?mode=ro"}
	default:
		// Linux and other Unix-like systems. The tray doesn't run on
		// all of these, but the package must compile and be testable
		// everywhere. This path is the documented default for DaVinci
		// Resolve on Linux.
		if home == "" {
			return nil
		}
		dbPath := filepath.Join(home, ".local", "share", "DaVinciResolve", "Support", "Manager", "Cache", "sqlite", "DaVinciResolveDatabase.db")
		return []string{"file:" + dbPath + "?mode=ro"}
	}
}

// DiscoverDatabaseURL probes each candidate URL with a brief Open+Close
// to find a reachable local Resolve database. Returns the first URL that
// opens successfully, or ("", nil) if none do. A candidate that exists
// but is corrupt or locked is treated the same as one that doesn't
// exist — not an error, just not usable.
//
// The probe re-opens on every call (no caching) so a database that
// becomes available after the agent starts — for example, DaVinci
// Resolve launched after the tray — is picked up on the next sync pass.
func DiscoverDatabaseURL(ctx context.Context, candidates []string) (string, error) {
	for _, u := range candidates {
		db, err := Open(ctx, u)
		if err != nil {
			continue
		}
		_ = db.Close()
		return u, nil
	}
	return "", nil
}

// DiscoverDatabaseURLForGOOS is DiscoverDatabaseURL parameterized on GOOS,
// home, and appData for table-testability, matching CandidateDirs's own
// injection convention. The production entry point is
// DiscoverDefaultDatabaseURL, which reads runtime.GOOS, os.UserHomeDir,
// and os.Getenv("LOCALAPPDATA").
func DiscoverDatabaseURLForGOOS(ctx context.Context, goos, home, appData string) (string, error) {
	return DiscoverDatabaseURL(ctx, CandidateDatabaseURLs(goos, home, appData))
}

// DefaultCandidateHome returns the home directory appropriate for the
// given GOOS, or "" if it cannot be determined. This is the production
// helper that CandidateDatabaseURLs's callers use to resolve the home
// parameter — separated so unit tests can inject synthetic values.
func DefaultCandidateHome() string {
	home, _ := os.UserHomeDir()
	return home
}

// DiscoverDefaultDatabaseURL is the production convenience wrapper:
// runtime.GOOS, DefaultCandidateHome(), os.Getenv("LOCALAPPDATA"),
// context.Background().
func DiscoverDefaultDatabaseURL(ctx context.Context) (string, error) {
	return DiscoverDatabaseURLForGOOS(ctx, runtime.GOOS, DefaultCandidateHome(), os.Getenv("LOCALAPPDATA"))
}
