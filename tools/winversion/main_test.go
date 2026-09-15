package main

import "testing"

func TestQuad(t *testing.T) {
	cases := map[string]string{
		"v1.6.0":      "1.6.0.0",
		"1.6.0":       "1.6.0.0",
		"v1.6.0-rc1":  "1.6.0.0",
		"v1.2.3.4":    "1.2.3.0", // BundleVersion truncates to 3 components before Quad pads
		"ci-check":    "0.0.0.0",
		"manual-test": "0.0.0.0",
		"":            "0.0.0.0",
	}
	for in, want := range cases {
		if got := Quad(in); got != want {
			t.Errorf("Quad(%q) = %q, want %q", in, got, want)
		}
	}
}
