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

// ErrSigstoreAttestationMissing is returned by Apply when a release
// asset's cosign-produced .sig or .cert is not present on the release
// (HTTP 404, or a 200 with empty body). This is expected for releases
// published before the workflow learned to sign in PR #131, and fatal
// for any release that should have been signed.
var ErrSigstoreAttestationMissing = errors.New("selfupdate: release asset's Sigstore attestation (.sig or .cert) is missing; this release was not built by a signing workflow")

// ErrSigstoreAttestationDownload is returned by Apply when fetching
// the .sig or .cert fails for any reason other than 404 -- a
// transient network error, a TLS handshake failure, a context
// cancellation. The underlying error is wrapped via %w.
var ErrSigstoreAttestationDownload = errors.New("selfupdate: failed to download release asset's Sigstore attestation; check network connectivity")

// ErrSigstoreVerificationFailed is returned by Apply when the Sigstore
// signature verification itself fails -- cert does not chain to a
// trusted Fulcio root, OIDC issuer does not match, SAN regex does
// not match, or the signature is not a valid ECDSA-P256 over SHA-256
// of the archive bytes. The underlying verify.ErrVerification (or
// any other sigstore-go error) is preserved via %w and can be
// inspected with errors.As.
var ErrSigstoreVerificationFailed = errors.New("selfupdate: Sigstore signature verification failed; refusing to apply update")
