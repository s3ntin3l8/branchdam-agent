// Package selfupdate verifies cosign-produced Sigstore signatures on
// release assets in-process via github.com/sigstore/sigstore-go.
// See selfupdate.go's package doc for the threat model and what is
// (and is not) checked.
//
// The trusted root is a hand-pruned subset of the sigstore-go public-good
// example, embedded at internal/selfupdate/trusted_root_public_good.json
// (Fulcio root + intermediate only -- Rekor/SCT/TSA sections are
// deliberately removed because the verify path skips those verifiers).
//
// Re-embedding cadence: when Fulcio rotates its root, re-fetch the
// upstream example, prune the same way, and re-commit
// trusted_root_public_good.json alongside a Sigstore rotation PR. Until
// then the embedded root is authoritative; a future release built
// against a rotated root would fail cert-chain verification against the
// stale embedded root. Track https://github.com/sigstore/Fulcio for
// rotation announcements.
package selfupdate

import "strings"

// sigSuffix is the .sig suffix cosign sign-blob --output-signature
// appends, and what gh release upload preserves. Must stay byte-identical
// to what .github/workflows/release-binaries.yml:228-240 emits.
const sigSuffix = ".sig"

// certSuffix is the .cert suffix cosign sign-blob --output-certificate
// appends. Same stability requirement as sigSuffix.
const certSuffix = ".cert"

// deriveSigURL returns the URL of <assetURL>.sig. The release workflow
// uploads the .sig and .cert alongside the original asset; the URL is
// deterministic from the asset URL (no need to look it up via the GitHub
// releases API, which would require a second round trip). A trailing
// '/' on assetURL is stripped first -- GitHub doesn't issue them today,
// but a hand-rolled release or future upload-step refactor might, and
// appending .sig to ".../asset/" would produce ".../asset/.sig" which 404s.
func deriveSigURL(assetURL string) string {
	return strings.TrimSuffix(assetURL, "/") + sigSuffix
}

// deriveCertURL returns the URL of <assetURL>.cert. See deriveSigURL
// for the rationale on the trailing-slash strip.
func deriveCertURL(assetURL string) string {
	return strings.TrimSuffix(assetURL, "/") + certSuffix
}
