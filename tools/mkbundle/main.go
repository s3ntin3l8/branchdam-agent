// Command mkbundle assembles a macOS .app bundle around a
// branchdam-agent binary, for use from the Makefile and from
// .github/workflows/release-binaries.yml's build-darwin job. All the
// actual bundle-shape logic lives in internal/appbundle -- this is a thin
// flag-parsing wrapper so CI and local builds share one implementation
// instead of a shell script re-deriving the layout.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("mkbundle", flag.ContinueOnError)
	appDir := fs.String("app", "", "output bundle path, e.g. dist/branchdam-agent.app")
	binPath := fs.String("binary", "", "path to the built branchdam-agent binary to bundle")
	ver := fs.String("version", "dev", "version string to render into Info.plist")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *appDir == "" || *binPath == "" {
		fmt.Fprintln(os.Stderr, "mkbundle: -app and -binary are required")
		fs.Usage()
		return 2
	}

	if err := appbundle.Write(*appDir, *binPath, *ver); err != nil {
		fmt.Fprintf(os.Stderr, "mkbundle: %v\n", err)
		return 1
	}
	fmt.Printf("mkbundle: wrote %s\n", *appDir)
	return 0
}
