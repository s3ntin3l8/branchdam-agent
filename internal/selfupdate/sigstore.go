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

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// sigSuffix is the .sig suffix cosign sign-blob --output-signature
// appends, and what gh release upload preserves. Must stay byte-identical
// to what .github/workflows/release-binaries.yml:228-240 emits.
const sigSuffix = ".sig"

// certSuffix is the .cert suffix cosign sign-blob --output-certificate
// appends. Same stability requirement as sigSuffix.
const certSuffix = ".cert"

// embeddedTrustedRoot is the Fulcio-only subset of the sigstore-go
// public-good example. See package doc for provenance.
//
//go:embed trusted_root_public_good.json
var embeddedTrustedRoot []byte

// sigstoreTrustedIdentity is the hard-coded OIDC issuer + SAN regex
// every signature must match. Built once at package init; an invalid
// regex is a programming error, not a runtime condition.
//
//   - OIDC issuer: https://token.actions.githubusercontent.com
//     (a Fulcio cert issued from any other Sigstore instance, e.g.
//     staging, is rejected)
//   - SAN regex: ^https://github.com/s3ntin3l8/branchdam-agent/
//     (a signature minted by a different repo's workflow, even one
//     that uses keyless correctly, is also rejected)
var sigstoreTrustedIdentity = mustNewTrustedCertIdentity()

func mustNewTrustedCertIdentity() verify.CertificateIdentity {
	id, err := verify.NewShortCertificateIdentity(
		"https://token.actions.githubusercontent.com",
		"", // issuerRegex (unused; the hard issuer above is the exact match)
		"", // sanValue (unused, the regex below is the only SAN criterion)
		`^https://github\.com/s3ntin3l8/branchdam-agent/`,
	)
	if err != nil {
		panic(fmt.Sprintf("selfupdate: hard-coded Sigstore trusted identity: %v", err))
	}
	return id
}

// cachedVerifier is built lazily on first use and reused for the
// process lifetime. The Verifier is safe for concurrent use per the
// sigstore-go docs.
var (
	verifierOnce      sync.Once
	cachedVerifier    *verify.Verifier
	cachedVerifierErr error
)

func loadVerifier() (*verify.Verifier, error) {
	verifierOnce.Do(func() {
		tr, err := root.NewTrustedRootFromJSON(embeddedTrustedRoot)
		if err != nil {
			cachedVerifierErr = fmt.Errorf("selfupdate: parse embedded trusted root: %w", err)
			return
		}
		// sigstore-go's Verifier requires *some* timestamp source to be
		// selected via NewVerifier options; the choices are
		// WithSignedTimestamps (RFC3161 TSA), WithObserverTimestamps
		// (TSA + Rekor SET), WithIntegratedTimestamps (Rekor SET only),
		// WithCurrentTime (use wall clock for chain validation), or
		// WithNoObserverTimestamps (key-only verification). Per the
		// package doc we deliberately skip Rekor/SCT/TSA, so the only
		// option that gives us "just sig + cert chain against current
		// time" is WithCurrentTime. This means a Fulcio cert is
		// validated with time.Now() as the verification time -- fine
		// for fresh releases; the embedded trusted root will be
		// re-cut on Fulcio root rotation (see package doc).
		v, err := verify.NewVerifier(tr, verify.WithCurrentTime())
		if err != nil {
			cachedVerifierErr = fmt.Errorf("selfupdate: build verifier: %w", err)
			return
		}
		cachedVerifier = v
	})
	return cachedVerifier, cachedVerifierErr
}

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

// decodeBase64OrPassThrough accepts either a raw base64 string (the
// default for cosign sign-blob --output-signature, including any
// trailing newline) or a raw byte string (what some test fixtures
// produce). It returns the raw signature bytes in either case.
func decodeBase64OrPassThrough(sig []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(sig)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty signature")
	}
	// base64.StdEncoding requires len % 4 == 0; signature length from
	// ECDSA-P256 over SHA-256 is 64 bytes -> 88 base64 chars with
	// padding, so this is the common case. Try decode; on failure
	// assume the input is already raw bytes.
	if decoded, err := base64.StdEncoding.DecodeString(string(trimmed)); err == nil && len(decoded) > 0 {
		return decoded, nil
	}
	return trimmed, nil
}

// buildSyntheticBundle constructs a sigstore-go Bundle protobuf from
// the archive bytes, the .sig bytes, and the .cert bytes. The shape
// matches what sigstore-go's test/conformance and protobuf-specs define
// for a MessageSignature with an X.509 Certificate verification material.
func buildSyntheticBundle(archive, sig, cert []byte) (*bundle.Bundle, error) {
	sigRaw, err := decodeBase64OrPassThrough(sig)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: decode .sig: %w", err)
	}

	// .cert is PEM. Expect exactly one CERTIFICATE block; reject chains
	// (the workflow does not use --certificate-chain).
	block, rest := pem.Decode(cert)
	if block == nil {
		return nil, fmt.Errorf("selfupdate: .cert is not PEM-encoded")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("selfupdate: .cert PEM block is %q, want CERTIFICATE", block.Type)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("selfupdate: .cert contains more than one PEM block; cosign --certificate-chain output is not supported")
	}

	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: parse .cert DER: %w", err)
	}

	digest := sha256.Sum256(archive)

	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle+json;version=0.3",
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digest[:],
				},
				Signature: sigRaw,
			},
		},
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{
					RawBytes: parsed.Raw,
				},
			},
		},
	}

	return bundle.NewBundle(pb)
}

// sigstoreVerify is the in-process verify. Returns nil on success,
// ErrSigstoreAttestationMissing if cert or sig is empty, or a wrapped
// error on failure (cert chain / issuer / SAN / sig check). The
// underlying verify.ErrVerification (or any other sigstore-go error)
// is preserved via %w / errors.As so the tray can log the specific
// reason. ErrSigstoreAttestationDownload is the caller's responsibility
// (the download happens in sigstorePreflight, not here).
func sigstoreVerify(archive, sig, cert []byte) error {
	return sigstoreVerifyWithIdentity(archive, sig, cert, sigstoreTrustedIdentity)
}

// sigstoreVerifyWithIdentity is the parameterized form. identity is
// a parameter so tests can supply a permissive test identity instead
// of the production-pinned one. Production callers should use
// sigstoreVerify above.
func sigstoreVerifyWithIdentity(archive, sig, cert []byte, identity verify.CertificateIdentity) error {
	if len(sig) == 0 || len(cert) == 0 {
		return fmt.Errorf("%w: .sig=%d bytes, .cert=%d bytes", ErrSigstoreAttestationMissing, len(sig), len(cert))
	}
	v, err := loadVerifier()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}
	b, err := buildSyntheticBundle(archive, sig, cert)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}
	_, err = v.Verify(b, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(archive)),
		verify.WithCertificateIdentity(identity),
	))
	if err != nil {
		// Always %w the underlying error so the tray can inspect it
		// with errors.Is / errors.As for any sigstore-go error type
		// (ErrVerification, ErrNoMatchingCertificateIdentity, etc.).
		// The user-facing message stays the constant.
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}
	return nil
}
