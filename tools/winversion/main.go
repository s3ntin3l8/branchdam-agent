// Command winversion prints a release tag as the strict 4-component
// numeric version string Windows resource metadata requires (NSIS's
// VIProductVersion/VIAddVersionKey, and any future .syso version resource --
// see FileVersion/ProductVersion in the Windows Explorer Properties >
// Details tab), given an arbitrary tag string that -- unlike Info.plist's
// CFBundleVersion -- may not be plain semver at all (e.g. "ci-check" or
// "manual-test" for a non-release build).
//
// Reuses internal/appbundle.BundleVersion rather than re-deriving the same
// "strip v, cut at -/+, keep numeric dot components" normalization a third
// time (internal/appbundle already does it for Info.plist); this just pads
// BundleVersion's 1-3 component result out to exactly 4, since Windows'
// version resource format requires all four (a bare "1.6.0" is rejected).
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr *os.File) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: winversion <tag>")
		return 2
	}
	_, _ = fmt.Fprintln(stdout, Quad(args[0]))
	return 0
}

// Quad normalizes tag to Windows' required X.X.X.X version resource
// format.
func Quad(tag string) string {
	parts := strings.Split(appbundle.BundleVersion(tag), ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".")
}
