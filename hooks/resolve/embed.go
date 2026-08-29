// Package resolve embeds branchdam_render_hook.py so the tray (and the
// headless `resolve-hook` subcommand) can install it into DaVinci
// Resolve's Scripts/Utility directory.
//
// This file lives HERE, next to the script, rather than under internal/,
// because go:embed cannot reference a path outside its own package
// directory. The alternative -- moving the .py under internal/ -- would
// break this directory's own README.md install instructions,
// .github/workflows/ci-python.yml's path filters and coverage-source, and
// the sibling test_branchdam_render_hook.py, all to satisfy a directory
// convention. Nothing here has any Resolve or Python dependency; it is a
// byte slice and its checksum.
package resolve

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

//go:embed branchdam_render_hook.py
var sourceFS embed.FS

// Source is the exact bytes of the render hook this binary ships.
var Source = func() []byte {
	b, err := sourceFS.ReadFile("branchdam_render_hook.py")
	if err != nil {
		// Unreachable in a successful build: go:embed already fails the
		// build itself if the file is missing, so a runtime error here
		// would mean the embed directive and this read disagree on the
		// filename -- a programming error, not an operator-facing one.
		panic("hooks/resolve: embedded branchdam_render_hook.py missing: " + err.Error())
	}
	return b
}()

// FileName is the name the script must have inside Scripts/Utility --
// Resolve's Workspace > Scripts menu lists it by filename.
const FileName = "branchdam_render_hook.py"

// SourceSHA256 is the hex checksum of the version this binary ships,
// computed once at init. Comparing it against an installed copy's own
// checksum is how internal/resolvehook.Detect distinguishes "installed
// and up to date" from "installed but modified or out of date" -- the two
// are indistinguishable by design (both are a checksum mismatch), so the
// UI says so rather than guessing which.
var SourceSHA256 = func() string {
	sum := sha256.Sum256(Source)
	return hex.EncodeToString(sum[:])
}()
