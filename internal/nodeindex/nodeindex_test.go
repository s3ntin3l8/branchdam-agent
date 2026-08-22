package nodeindex

import (
	"os"
	"path/filepath"
	"testing"
)

func writeIndexFile(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "node-index.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write index file: %v", err)
	}
	return path
}

func TestLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexFile(t, dir, `{
		"/masters/DSC_0001.NEF": "0198f2c1-2e3a-7c9e-8b1a-1234567890ab"
	}`)

	idx, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", idx.Len())
	}

	uuid, ok, err := idx.Resolve("/masters/DSC_0001.NEF")
	if err != nil {
		t.Fatalf("Resolve: unexpected error %v", err)
	}
	if !ok {
		t.Fatal("Resolve: expected ok=true for a known path")
	}
	if uuid != "0198f2c1-2e3a-7c9e-8b1a-1234567890ab" {
		t.Errorf("Resolve: uuid = %q, want the seeded value", uuid)
	}

	_, ok, err = idx.Resolve("/masters/unknown.NEF")
	if err != nil {
		t.Fatalf("Resolve: unexpected error %v", err)
	}
	if ok {
		t.Error("Resolve: expected ok=false for an unknown path")
	}
}

func TestLoadEmptyObject(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexFile(t, dir, `{}`)

	idx, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Len() != 0 {
		t.Errorf("Len() = %d, want 0", idx.Len())
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error loading a missing index file, got nil")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexFile(t, dir, `not json`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error loading malformed JSON, got nil")
	}
}
