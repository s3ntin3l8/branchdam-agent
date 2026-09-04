//go:build linux

package eject

import (
	"testing"
)

func TestResolveDeviceFromMounts(t *testing.T) {
	mounts := []byte(`/dev/sda1 / ext4 rw,relatime 0 0
/dev/sdb1 /media/user/CANON\040R5 vfat rw,nosuid,nodev,relatime,uid=1000 0 0
/dev/sdc1 /media/user/SONY_A7IV vfat rw,nosuid,nodev 0 0
tmpfs /run/user/1000 tmpfs rw,nosuid,nodev 0 0
`)

	tests := []struct {
		name      string
		mountPath string
		wantDev   string
		wantErr   bool
	}{
		{
			name:      "exact match with space in name",
			mountPath: "/media/user/CANON R5",
			wantDev:   "/dev/sdb1",
			wantErr:   false,
		},
		{
			name:      "exact match with trailing slash",
			mountPath: "/media/user/SONY_A7IV/",
			wantDev:   "/dev/sdc1",
			wantErr:   false,
		},
		{
			name:      "subdirectory within mount",
			mountPath: "/media/user/CANON R5/DCIM/100CANON",
			wantDev:   "/dev/sdb1",
			wantErr:   false,
		},
		{
			name:      "unmounted path",
			mountPath: "/media/user/UNKNOWN",
			wantDev:   "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDev, err := resolveDeviceFromMounts(tt.mountPath, mounts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveDeviceFromMounts(%q) error = %v, wantErr %v", tt.mountPath, err, tt.wantErr)
			}
			if gotDev != tt.wantDev {
				t.Errorf("resolveDeviceFromMounts(%q) = %q, want %q", tt.mountPath, gotDev, tt.wantDev)
			}
		})
	}
}

func TestUnescapeOctal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: `no_escapes`, want: `no_escapes`},
		{input: `/media/user/CANON\040R5`, want: `/media/user/CANON R5`},
		{input: `/path/with\011tab`, want: "/path/with\ttab"},
		{input: `/path/with\134backslash`, want: `/path/with\backslash`},
		{input: `/path/\invalid`, want: `/path/\invalid`},
		{input: `/path/\777outofrange`, want: `/path/\777outofrange`},
	}

	for _, tt := range tests {
		got := unescapeOctal(tt.input)
		if got != tt.want {
			t.Errorf("unescapeOctal(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
