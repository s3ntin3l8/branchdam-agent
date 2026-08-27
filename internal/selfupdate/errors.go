package selfupdate

import "errors"

// ErrVersionNotSemver is returned by Check/Apply when the running binary's
// version string is not parseable semver -- "dev" for a plain `go build`/
// `make build`, or for `make build-windows`/`build-darwin-app` run without
// an explicit VERSION=<semver>. go-selfupdate's own Release.GreaterThan
// panics on a non-semver argument (it calls semver.MustParse internally),
// so this check runs before any call into go-selfupdate, not after.
var ErrVersionNotSemver = errors.New("selfupdate: running version is not a released semver version (a locally built binary reports \"dev\"; only a release build can self-update)")

// ErrNotNewer is returned by Apply when the latest release is not
// strictly newer than the running version. go-selfupdate's own
// UpdateCommand helper only checks Equal, which would let a re-tagged or
// yanked release downgrade a workstation; this package never calls
// UpdateCommand and always re-asserts GreaterThan itself.
var ErrNotNewer = errors.New("selfupdate: latest release is not newer than the running version")

// ErrTargetNotWritable is returned by Apply when a target's directory
// isn't writable by the current user -- checked before any download, so a
// system-wide install (root-owned /Applications, Administrator-owned
// C:\Program Files\) fails fast with a clear message instead of after a
// multi-megabyte download.
var ErrTargetNotWritable = errors.New("selfupdate: install directory is not writable; self-update requires a per-user install location")

// ErrTranslocated is returned by DetectLayout when the running executable
// resolves to a macOS App Translocation path. Such a path is a randomized,
// read-only mount that disappears on reboot -- self-update cannot write
// there, and baking it into a login item would register a path that stops
// existing. Move the .app to /Applications or ~/Applications to clear it.
var ErrTranslocated = errors.New("selfupdate: running from a Gatekeeper-translocated path; move the app to /Applications or ~/Applications first")
