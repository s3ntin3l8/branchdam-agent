// Command mkicon writes internal/appicon's rendered .ico to disk, for use
// from the Makefile and from .github/workflows/*.yml's Windows build jobs
// -- the same "generate the binary asset at build time, commit no binary
// asset" pattern tools/mkbundle already uses for the macOS .icns, mirrored
// here rather than shared since the two formats' generation (appbundle.Write
// vs. a bare file write) don't have enough in common to be worth a shared
// abstraction over.
package main

import (
	"fmt"
	"os"

	"github.com/s3ntin3l8/branchdam-agent/internal/appicon"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: mkicon <output.ico>")
		return 2
	}
	out := args[0]

	data, err := appicon.ICO()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkicon: %v\n", err)
		return 1
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "mkicon: %v\n", err)
		return 1
	}
	fmt.Printf("mkicon: wrote %s\n", out)
	return 0
}
