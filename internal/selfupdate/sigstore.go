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
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	su "github.com/creativeprojects/go-selfupdate"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
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
//   - SAN regex: ^https://github\.com/s3ntin3l8/branchdam-agent/\.github/workflows/release-binaries\.yml@
//     pinned to the exact trusted workflow file. A signature minted
//     by any other workflow (e.g. a future PR-triggered workflow
//     that also has `id-token: write`) is rejected.
var sigstoreTrustedIdentity = mustNewTrustedCertIdentity()

func mustNewTrustedCertIdentity() verify.CertificateIdentity {
	id, err := verify.NewShortCertificateIdentity(
		"https://token.actions.githubusercontent.com",
		"", // issuerRegex (unused; the hard issuer above is the exact match)
		"", // sanValue (unused, the regex below is the only SAN criterion)
		`^https://github\.com/s3ntin3l8/branchdam-agent/\.github/workflows/release-binaries\.yml@`,
	)
	if err != nil {
		panic(fmt.Sprintf("selfupdate: hard-coded Sigstore trusted identity: %v", err))
	}
	return id
}

// trustedRootOnce ensures the trusted root is parsed exactly once
// across all calls. TrustedRoot is safe for concurrent use per the
// sigstore-go docs.
var (
	trustedRootOnce   sync.Once
	cachedTrustedRoot *root.TrustedRoot
	cachedTrustedErr  error
)

// loadTrustedRoot parses the embedded trusted root. Done lazily so a
// parse failure (e.g. a botched rotation) doesn't take down package
// init for the whole binary -- the failure surfaces on first Apply.
func loadTrustedRoot() (*root.TrustedRoot, error) {
	trustedRootOnce.Do(func() {
		tr, err := root.NewTrustedRootFromJSON(embeddedTrustedRoot)
		if err != nil {
			cachedTrustedErr = fmt.Errorf("selfupdate: parse embedded trusted root: %w", err)
			return
		}
		cachedTrustedRoot = tr
	})
	return cachedTrustedRoot, cachedTrustedErr
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

// sigstoreVerify is the in-process Sigstore keyless-signature
// verification. Returns nil on success, ErrSigstoreAttestationMissing
// if cert or sig is empty, or a wrapped error on failure.
//
// This deliberately bypasses sigstore-go's high-level Verifier. The
// Verifier forces the cert chain validation time to be either
// time.Now() (rejects short-lived Fulcio certs the moment their 10-min
// NotAfter expires, which breaks every self-update that runs more
// than 10 minutes after a release) or a timestamp from a TSA / Rekor
// entry (which requires a workflow change to upload and is out of
// scope for issue #156). The Sigstore spec for cert path validation
// explicitly allows "a fake 'current time'" for the no-timestamp
// case; we use the cert's own NotBefore, which is guaranteed to be
// inside the cert's validity window by construction.
//
// The threat model is unchanged: we still prove (a) the cert chains
// to the embedded Fulcio root, (b) the cert's OIDC issuer matches
// the GitHub Actions OIDC issuer, (c) the cert's SAN matches the
// expected workflow, and (d) the signature is a valid ECDSA-P256 over
// SHA-256 of the archive bytes. We do NOT prove the signing event
// happened at a specific time -- that requires Rekor/TSA and is
// tracked separately.
func sigstoreVerify(archive, sig, cert []byte) error {
	if len(sig) == 0 || len(cert) == 0 {
		return fmt.Errorf("%w: .sig=%d bytes, .cert=%d bytes", ErrSigstoreAttestationMissing, len(sig), len(cert))
	}
	return verifyAttestation(archive, sig, cert, sigstoreTrustedIdentity)
}

// verifyAttestation is the core verify logic, extracted so tests can
// pass a permissive test identity (see sigstore_test.go). Production
// callers should use sigstoreVerify above.
func verifyAttestation(archive, sig, cert []byte, identity verify.CertificateIdentity) error {
	// Decode the leaf cert. We accept only a single PEM block (the
	// release workflow uses cosign sign-blob --output-certificate
	// without --certificate-chain, so the .cert is exactly one leaf).
	leaf, err := parseLeafCert(cert)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}

	// Decode the signature. The release workflow's .sig is the
	// base64-encoded ECDSA-P256 ASN.1 DER; sigstoreVerifyWithIdentity
	// would normally do this via the synthetic bundle, but we need
	// the raw bytes for the ecdsa.VerifyASN1 call below.
	sigRaw, err := decodeBase64OrPassThrough(sig)
	if err != nil {
		return fmt.Errorf("%w: decode .sig: %w", ErrSigstoreVerificationFailed, err)
	}

	// Cert chain validation at the cert's own NotBefore. Fulcio
	// short-lived certs are valid for ~10 minutes; we validate the
	// chain at the earliest moment the cert is valid, which by
	// construction is inside the cert's validity window. This is
	// the "fake current time" path the Sigstore spec allows.
	tr, err := loadTrustedRoot()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}
	if err := verifyCertChain(leaf, tr, leaf.NotBefore); err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}

	// Identity check: OIDC issuer + SAN regex against the cert's
	// summary. CertificateIdentities.Verify returns the matching
	// identity (or an error) -- we use it to reuse sigstore-go's
	// matchers (regex compilation, SAN URL extraction, etc.) rather
	// than reimplementing them.
	summary, err := certificate.SummarizeCertificate(leaf)
	if err != nil {
		return fmt.Errorf("%w: summarize cert: %w", ErrSigstoreVerificationFailed, err)
	}
	identities := verify.CertificateIdentities{identity}
	if _, err := identities.Verify(summary); err != nil {
		return fmt.Errorf("%w: %w", ErrSigstoreVerificationFailed, err)
	}

	// Signature verification: ECDSA-P256 over SHA-256(archive)
	// against the leaf cert's public key. This is a plain
	// cryptographic check -- no time component -- and is what
	// proves the cert actually signed the bytes we're about to apply.
	digest := sha256.Sum256(archive)
	ecPub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: leaf cert public key is %T, expected *ecdsa.PublicKey", ErrSigstoreVerificationFailed, leaf.PublicKey)
	}
	if !ecdsa.VerifyASN1(ecPub, digest[:], sigRaw) {
		return fmt.Errorf("%w: ECDSA signature does not match archive", ErrSigstoreVerificationFailed)
	}

	return nil
}

// parseLeafCert decodes a single PEM CERTIFICATE block. The release
// workflow emits exactly one block (cosign sign-blob --output-certificate
// without --certificate-chain), so we reject anything else.
func parseLeafCert(pemBytes []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf(".cert is not PEM-encoded")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf(".cert PEM block is %q, want CERTIFICATE", block.Type)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf(".cert contains more than one PEM block; cosign --certificate-chain output is not supported")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse .cert DER: %w", err)
	}
	return cert, nil
}

// verifyCertChain iterates the trusted root's Fulcio CAs and returns
// nil on the first one that successfully chains the leaf at the
// supplied observerTimestamp. The timestamp is the cert's own
// NotBefore (see sigstoreVerify's doc comment for why).
func verifyCertChain(leaf *x509.Certificate, tr *root.TrustedRoot, observerTimestamp time.Time) error {
	for _, ca := range tr.FulcioCertificateAuthorities() {
		if _, err := ca.Verify(leaf, observerTimestamp); err == nil {
			return nil
		}
	}
	return fmt.Errorf("leaf certificate does not chain to any trusted Fulcio root at %s", observerTimestamp.Format(time.RFC3339))
}

// attestCache holds the .sig + .cert bytes fetched during
// sigstorePreflight, keyed by the asset Name go-selfupdate passes to
// the per-target Validator (i.e. release.AssetName). Lives for the
// duration of one Apply call. The per-target Validator reads from
// this cache via attestCache.lookup(name).
type attestCache struct {
	mu     sync.Mutex
	byName map[string]attestPair
}

type attestPair struct {
	sig, cert []byte
}

func newAttestCache() *attestCache {
	return &attestCache{byName: make(map[string]attestPair)}
}

func (c *attestCache) put(name string, sig, cert []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byName[name] = attestPair{sig: sig, cert: cert}
}

func (c *attestCache) lookup(name string) (sig, cert []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byName[name]
	return p.sig, p.cert, ok
}

// assetAttestation pairs the asset name (cache key) with the URL
// (where to fetch .sig and .cert from). Each go-selfupdate target
// gets its own pair, but all targets in one Apply share the same
// (Name, URL) because they all download the same release archive.
type assetAttestation struct {
	Name string
	URL  string
}

// fetchClient is the HTTP client used for .sig/.cert downloads. It
// has a Timeout to avoid an indefinite block on a stalled CDN
// connection (http.DefaultClient has no timeout). 30s is generous
// for a few-KB GET but bounded so a network stall surfaces
// quickly. ctx cancellation still wins.
var fetchClient = &http.Client{Timeout: 30 * time.Second}

// fetchAttestation GETs url with ctx. Returns the body, nil on
// 2xx; nil + nil on 404 (caller treats 404 as "missing"); non-nil
// error on any other non-2xx or transport failure.
func fetchAttestation(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub release-asset CDN requires a User-Agent (returns 403
	// without one) and accepts If-None-Match / Range but we don't
	// need them -- a fresh GET is fine for a few-KB .sig/.cert.
	req.Header.Set("User-Agent", "branchdam-agent-selfupdate")
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := fetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// sigstorePreflight fetches the .sig and .cert for every named asset,
// populating the cache for later per-target verify. Returns nil on
// success; one of ErrSigstoreAttestationMissing or
// ErrSigstoreAttestationDownload on failure.
//
// Called once per Apply, BEFORE the sidecar-write step (so a failed
// preflight never leaves a "previous version" sidecar orphaning an
// unapplied-update backup) and BEFORE any per-target UpdateTo.
func sigstorePreflight(ctx context.Context, assets []assetAttestation, cache *attestCache) error {
	for _, a := range assets {
		// Skip the network round trip if this asset was already
		// populated (multiple targets share the same archive).
		if _, _, ok := cache.lookup(a.Name); ok {
			continue
		}
		sig, err := fetchAttestation(ctx, deriveSigURL(a.URL))
		if err != nil {
			return fmt.Errorf("%w: .sig for %s: %w", ErrSigstoreAttestationDownload, a.Name, err)
		}
		if sig == nil {
			return fmt.Errorf("%w: .sig for %s", ErrSigstoreAttestationMissing, a.Name)
		}
		cert, err := fetchAttestation(ctx, deriveCertURL(a.URL))
		if err != nil {
			return fmt.Errorf("%w: .cert for %s: %w", ErrSigstoreAttestationDownload, a.Name, err)
		}
		if cert == nil {
			return fmt.Errorf("%w: .cert for %s", ErrSigstoreAttestationMissing, a.Name)
		}
		cache.put(a.Name, sig, cert)
	}
	return nil
}

// sigstoreValidator implements go-selfupdate's Validator interface. It
// runs the SHA256 ChecksumValidator first, then sigstoreVerify. Either
// failure aborts Apply.
//
// go-selfupdate's Validator interface is a single Validate(filename,
// release, asset) method plus a GetValidationAssetName that tells
// DetectLatest/DetectVersion which sibling asset to download. We
// re-run the (cheap) Sigstore verify on each call rather than caching
// a pass/fail, matching the per-target ChecksumValidator pattern in
// the existing Apply loop.
type sigstoreValidator struct {
	cache *attestCache
}

// Compile-time check that sigstoreValidator satisfies go-selfupdate's
// Validator. The interface lives in creativeprojects/go-selfupdate
// (imported as su at the top of this file); sigstoreValidator is
// referenced by interface at the Apply call site via su.Config.Validator.
var _ su.Validator = (*sigstoreValidator)(nil)

// GetValidationAssetName returns the SHA256SUMS.txt asset name so
// go-selfupdate downloads it during DetectLatest/DetectVersion. The
// Sigstore .sig/.cert are fetched out-of-band by sigstorePreflight
// in Apply, not by DetectLatest; this method only needs to tell
// go-selfupdate about the SHA256SUMS sibling.
func (v *sigstoreValidator) GetValidationAssetName(releaseFilename string) string {
	return ChecksumAsset
}

func (v *sigstoreValidator) Validate(filename string, release, asset []byte) error {
	// SHA256 first (existing behavior). This is identical to
	// ChecksumValidator.Validate; we re-derive instead of delegating
	// to avoid a second import path through go-selfupdate's source.
	sha := &su.ChecksumValidator{UniqueFilename: ChecksumAsset}
	if err := sha.Validate(filename, release, asset); err != nil {
		return err
	}
	// Sigstore second.
	sig, cert, ok := v.cache.lookup(filename)
	if !ok {
		// The per-target Validator should only be called for
		// assets that sigstorePreflight populated. If it isn't,
		// that's a programming error in Apply (we forgot to
		// preflight, or preflight didn't run for this asset name).
		return fmt.Errorf("%w: no preflighted attestation for %s", ErrSigstoreAttestationMissing, filename)
	}
	return sigstoreVerify(release, sig, cert)
}
