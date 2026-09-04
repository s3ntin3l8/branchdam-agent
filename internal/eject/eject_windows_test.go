//go:build windows

package eject

import (
	"testing"
)

func TestVolumeDevicePath(t *testing.T) {
	tests := []struct {
		mountPath string
		want      string
		wantErr   bool
	}{
		{
			mountPath: `D:\`,
			want:      `\\.\D:`,
			wantErr:   false,
		},
		{
			mountPath: `E:\DCIM\100EOSR5`,
			want:      `\\.\E:`,
			wantErr:   false,
		},
		{
			mountPath: `F:`,
			want:      `\\.\F:`,
			wantErr:   false,
		},
		{
			mountPath: `\\.\G:`,
			want:      `\\.\G:`,
			wantErr:   false,
		},
		{
			mountPath: ``,
			want:      "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		got, err := volumeDevicePath(tt.mountPath)
		if (err != nil) != tt.wantErr {
			t.Fatalf("volumeDevicePath(%q) error = %v, wantErr %v", tt.mountPath, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("volumeDevicePath(%q) = %q, want %q", tt.mountPath, got, tt.want)
		}
	}
}
