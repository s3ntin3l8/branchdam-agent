package resolve

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAgentID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("operator-set wins", func(t *testing.T) {
		id, err := resolveAgentID("operator-id", "/nonexistent", "/nonexistent", logger)
		if err != nil {
			t.Fatal(err)
		}
		if id != "operator-id" {
			t.Errorf("got %q, want %q", id, "operator-id")
		}
	})

	t.Run("machine-id fallback", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "machine-id")
		if err := os.WriteFile(tmp, []byte("machine-abc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		id, err := resolveAgentID("", tmp, "/nonexistent", logger)
		if err != nil {
			t.Fatal(err)
		}
		if id != "machine-abc" {
			t.Errorf("got %q, want machine-abc", id)
		}
	})

	t.Run("persistent-id fallback", func(t *testing.T) {
		persistent := filepath.Join(t.TempDir(), "resolve-install-id")
		if err := os.WriteFile(persistent, []byte("persisted-xyz\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		id, err := resolveAgentID("", "/nonexistent", persistent, logger)
		if err != nil {
			t.Fatal(err)
		}
		if id != "persisted-xyz" {
			t.Errorf("got %q, want persisted-xyz", id)
		}
	})

	t.Run("generates and persists", func(t *testing.T) {
		persistent := filepath.Join(t.TempDir(), "resolve-install-id")
		id1, err := resolveAgentID("", "/nonexistent", persistent, logger)
		if err != nil {
			t.Fatal(err)
		}
		if id1 == "" {
			t.Fatal("empty generated id")
		}
		// Second call reads the file rather than minting again.
		id2, err := resolveAgentID("", "/nonexistent", persistent, logger)
		if err != nil {
			t.Fatal(err)
		}
		if id1 != id2 {
			t.Errorf("persistent id should be stable: %q vs %q", id1, id2)
		}
	})
}
