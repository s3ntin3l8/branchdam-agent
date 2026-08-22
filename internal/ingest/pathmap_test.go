package ingest

import (
	"errors"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

func TestToContainerPath(t *testing.T) {
	mappings := []config.PathMapping{
		{WorkstationPath: "/mnt/nas/archive", ContainerPath: "/storage/archive"},
		{WorkstationPath: "/mnt/nas/archive/canon", ContainerPath: "/storage/archive-canon"},
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"exact root", "/mnt/nas/archive", "/storage/archive"},
		{"nested file", "/mnt/nas/archive/2026/photo.jpg", "/storage/archive/2026/photo.jpg"},
		{"longest prefix wins", "/mnt/nas/archive/canon/2026/photo.jpg", "/storage/archive-canon/2026/photo.jpg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ToContainerPath(mappings, c.in)
			if err != nil {
				t.Fatalf("ToContainerPath: %v", err)
			}
			if got != c.want {
				t.Errorf("ToContainerPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestToContainerPathNoMapping(t *testing.T) {
	_, err := ToContainerPath(nil, "/mnt/nas/archive/photo.jpg")
	if !errors.Is(err, ErrNoPathMapping) {
		t.Fatalf("err = %v, want ErrNoPathMapping", err)
	}
}

// TestToContainerPathTrailingSlashInMapping is the regression test for a
// bug caught during self-review: a configured WorkstationPath carrying a
// trailing slash used to fail to equality-match a lookup of the bare root
// path (no trailing slash), since only the prefix-match branch trimmed the
// slash before comparing. Both directions -- mapping has a trailing slash,
// mapping has none -- must resolve identically.
func TestToContainerPathTrailingSlashInMapping(t *testing.T) {
	cases := []struct {
		name            string
		workstationPath string
	}{
		{"mapping has trailing slash", "/mnt/nas/archive/"},
		{"mapping has no trailing slash", "/mnt/nas/archive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mappings := []config.PathMapping{
				{WorkstationPath: c.workstationPath, ContainerPath: "/storage/archive"},
			}
			got, err := ToContainerPath(mappings, "/mnt/nas/archive")
			if err != nil {
				t.Fatalf("ToContainerPath: %v", err)
			}
			if got != "/storage/archive" {
				t.Errorf("ToContainerPath(bare root) = %q, want /storage/archive", got)
			}
		})
	}
}
