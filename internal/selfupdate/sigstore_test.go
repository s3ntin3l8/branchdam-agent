package selfupdate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

func TestDeriveSigAndCertURL(t *testing.T) {
	cases := []struct {
		name, asset, wantSig, wantCert string
	}{
		{
			name:     "linux tarball",
			asset:    "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz",
			wantSig:  "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz.sig",
			wantCert: "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz.cert",
		},
		{
			name:     "windows zip",
			asset:    "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-windows-amd64.zip",
			wantSig:  "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-windows-amd64.zip.sig",
			wantCert: "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-windows-amd64.zip.cert",
		},
		{
			name:     "SHA256SUMS",
			asset:    "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/SHA256SUMS.txt",
			wantSig:  "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/SHA256SUMS.txt.sig",
			wantCert: "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/SHA256SUMS.txt.cert",
		},
		{
			// Trailing slash should be stripped before .sig/.cert are
			// appended; otherwise the result is ".../asset/.sig" which
			// 404s. The trim happens inside the helper so the test
			// must cover the case explicitly.
			name:     "asset with trailing slash",
			asset:    "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz/",
			wantSig:  "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz.sig",
			wantCert: "https://github.com/s3ntin3l8/branchdam-agent/releases/download/v1.2.3/branchdam-agent-linux-amd64.tar.gz.cert",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSigURL(tc.asset); got != tc.wantSig {
				t.Errorf("deriveSigURL(%q) = %q, want %q", tc.asset, got, tc.wantSig)
			}
			if got := deriveCertURL(tc.asset); got != tc.wantCert {
				t.Errorf("deriveCertURL(%q) = %q, want %q", tc.asset, got, tc.wantCert)
			}
		})
	}
}

// testCertKeypair returns a self-signed ECDSA P-256 cert + its private
// key, suitable for signing an artifact but NOT for issuing other
// certs (BasicConstraintsValid + IsCA=false). The cert has a minimal
// URI SAN (just enough to satisfy sigstore-go's SummarizeCertificate,
// which requires SOME SAN) but no Fulcio OIDC extension. Tests that
// exercise the production identity policy should add extensions
// explicitly.
func testCertKeypair(t *testing.T, commonName string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{{Scheme: "https", Host: "test.invalid"}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, priv
}

// testCAKeypair returns a self-signed CA cert + its private key, with
// IsCA=true and KeyUsageCertSign set.
func testCAKeypair(t *testing.T, commonName string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, priv
}

// signLeafWithCA issues a leaf cert from a CA and returns the leaf
// cert + its private key. The leaf gets a minimal URI SAN (just
// enough to satisfy sigstore-go's SummarizeCertificate) and no Fulcio
// extensions. The permissiveTestIdentity in this file uses regex ".*"
// so the SAN can be anything.
func signLeafWithCA(t *testing.T, ca *x509.Certificate, caPriv *ecdsa.PrivateKey, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{{Scheme: "https", Host: "test.invalid"}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, leafPriv
}

// signECDSAP256SHA256 returns the raw ECDSA-P256 signature over
// SHA-256(message). Matches the .sig format cosign sign-blob produces
// (before base64 encoding).
func signECDSAP256SHA256(t *testing.T, priv *ecdsa.PrivateKey, message []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(message)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// encodeCertPEM returns the .cert PEM bytes cosign would produce.
func encodeCertPEM(t *testing.T, cert *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// encodeSigBase64 returns the .sig bytes cosign would produce (raw
// signature bytes, base64-encoded with std encoding, trailing newline).
func encodeSigBase64(t *testing.T, sig []byte) []byte {
	t.Helper()
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// permissiveTestIdentity is a CertificateIdentity that matches any
// cert. Used by the sigstoreVerify tests so the test fixtures can be
// minimal (no SAN, no Fulcio OIDC extension) and the tests exercise
// just the chain + signature plumbing, not the production policy.
//
// sigstore-go's NewShortCertificateIdentity requires at least one of
// issuer/sanValue (regex/value pairs); the regex ".*" matches anything.
var permissiveTestIdentity = mustNewPermissiveIdentity()

func mustNewPermissiveIdentity() verify.CertificateIdentity {
	id, err := verify.NewShortCertificateIdentity(
		"", // issuer (empty -- regex-only)
		".*",
		"",
		".*",
	)
	if err != nil {
		panic(err)
	}
	return id
}

// withTestTrustedRoot swaps the package-global cachedTrustedRoot for
// one built from a test CA, runs fn, and restores on cleanup. Use
// this when you need a TrustedRoot that doesn't include the production
// public-good Fulcio (which can't be replayed offline -- certs are
// short-lived).
func withTestTrustedRoot(t *testing.T, ca *x509.Certificate) {
	t.Helper()
	authority := &root.FulcioCertificateAuthority{
		Root:          ca,
		Intermediates: nil,
	}
	tr, err := root.NewTrustedRoot(
		root.TrustedRootMediaType01,
		[]root.CertificateAuthority{authority},
		nil, // ctLogs
		nil, // timestampAuthorities
		nil, // transparencyLogs (Rekor)
	)
	if err != nil {
		t.Fatal(err)
	}

	// Swap. The package's sync.Once makes normal swap impossible;
	// we just directly assign (test-only). The production trusted
	// root cache is never used by this test.
	saved := cachedTrustedRoot
	savedErr := cachedTrustedErr
	cachedTrustedRoot = tr
	cachedTrustedErr = nil
	t.Cleanup(func() {
		cachedTrustedRoot = saved
		cachedTrustedErr = savedErr
	})
}

// TestSigstoreVerifyRejectsUntrustedCert proves the verify actually
// verifies: a self-signed cert (not chained to the test CA in the
// trusted root) must produce ErrSigstoreVerificationFailed.
func TestSigstoreVerifyRejectsUntrustedCert(t *testing.T) {
	ca, _ := testCAKeypair(t, "test CA")
	withTestTrustedRoot(t, ca)

	archive := []byte("fake release archive bytes")
	cert, priv := testCertKeypair(t, "untrusted-test")
	sig := signECDSAP256SHA256(t, priv, archive)

	err := verifyAttestation(archive, encodeSigBase64(t, sig), encodeCertPEM(t, cert), permissiveTestIdentity)
	if err == nil {
		t.Fatal("sigstoreVerify accepted an untrusted cert; want ErrSigstoreVerificationFailed")
	}
	if !errors.Is(err, ErrSigstoreVerificationFailed) {
		t.Errorf("err = %v, want errors.Is ErrSigstoreVerificationFailed", err)
	}
}

// TestSigstoreVerifyAcceptsTestCA proves the cert-chain + signature
// plumbing is correct end-to-end, using a test CA + leaf cert signed
// by that CA. A real Fulcio-issued cert is short-lived and cannot be
// replayed offline, so the only way to assert "the verify actually
// works when the cert chains correctly" is via a test CA.
//
// The test uses verifyAttestation with a permissive identity (not the
// production sigstoreTrustedIdentity) so the test certs can be
// minimal -- no Fulcio OIDC extension. This isolates the chain-of-
// trust check from the identity check; the production identity
// policy is exercised end-to-end at the next real release.
func TestSigstoreVerifyAcceptsTestCA(t *testing.T) {
	ca, caPriv := testCAKeypair(t, "test CA")
	leaf, leafPriv := signLeafWithCA(t, ca, caPriv, "test leaf")
	archive := []byte("fake release archive bytes for known-good test")
	sig := signECDSAP256SHA256(t, leafPriv, archive)

	withTestTrustedRoot(t, ca)

	err := verifyAttestation(archive, encodeSigBase64(t, sig), encodeCertPEM(t, leaf), permissiveTestIdentity)
	if err != nil {
		t.Fatalf("sigstoreVerify rejected a cert chained to the test CA: %v", err)
	}
}

// TestSigstoreVerifyRejectsTamperedArchive proves the signature
// cryptographically ties the .cert to the bytes it signed. The same
// cert+sig from TestSigstoreVerifyAcceptsTestCA, but with a different
// archive passed in. Must fail with ErrSigstoreVerificationFailed.
func TestSigstoreVerifyRejectsTamperedArchive(t *testing.T) {
	ca, caPriv := testCAKeypair(t, "tamper-test CA")
	leaf, leafPriv := signLeafWithCA(t, ca, caPriv, "tamper-test leaf")
	signedArchive := []byte("the bytes we actually signed")
	tamperedArchive := []byte("DIFFERENT bytes, the attacker swapped these in")
	sig := signECDSAP256SHA256(t, leafPriv, signedArchive)

	withTestTrustedRoot(t, ca)

	err := verifyAttestation(tamperedArchive, encodeSigBase64(t, sig), encodeCertPEM(t, leaf), permissiveTestIdentity)
	if err == nil {
		t.Fatal("sigstoreVerify accepted a tampered archive; want ErrSigstoreVerificationFailed")
	}
	if !errors.Is(err, ErrSigstoreVerificationFailed) {
		t.Errorf("err = %v, want errors.Is ErrSigstoreVerificationFailed", err)
	}
}

// TestSigstoreVerifyEmptyInput covers the "empty sig / empty cert"
// paths. Both are reported as ErrSigstoreAttestationMissing (the
// download would have surfaced a 404, but the function should still
// fail closed if called with an empty body somehow -- e.g. the
// per-target Validator being invoked before preflight completes).
func TestSigstoreVerifyEmptyInput(t *testing.T) {
	t.Run("empty sig", func(t *testing.T) {
		err := sigstoreVerify([]byte("archive"), nil, []byte("cert"))
		if !errors.Is(err, ErrSigstoreAttestationMissing) {
			t.Errorf("err = %v, want ErrSigstoreAttestationMissing", err)
		}
	})
	t.Run("empty cert", func(t *testing.T) {
		err := sigstoreVerify([]byte("archive"), []byte("sig"), nil)
		if !errors.Is(err, ErrSigstoreAttestationMissing) {
			t.Errorf("err = %v, want ErrSigstoreAttestationMissing", err)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		err := sigstoreVerify([]byte("archive"), nil, nil)
		if !errors.Is(err, ErrSigstoreAttestationMissing) {
			t.Errorf("err = %v, want ErrSigstoreAttestationMissing", err)
		}
	})
}

// TestSigstorePreflightHappyPath spins up a local httptest server
// that serves .sig and .cert for a known asset URL, runs
// sigstorePreflight, and asserts the cache is populated.
func TestSigstorePreflightHappyPath(t *testing.T) {
	const (
		sigBody  = "fake-sig-bytes"
		certBody = "fake-cert-bytes"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/v1.2.3/branchdam-agent-linux-amd64.tar.gz.sig":
			_, _ = w.Write([]byte(sigBody))
		case "/releases/v1.2.3/branchdam-agent-linux-amd64.tar.gz.cert":
			_, _ = w.Write([]byte(certBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	assetURL := srv.URL + "/releases/v1.2.3/branchdam-agent-linux-amd64.tar.gz"
	cache := newAttestCache()
	err := sigstorePreflight(context.Background(), []assetAttestation{
		{Name: "branchdam-agent-linux-amd64.tar.gz", URL: assetURL},
	}, cache)
	if err != nil {
		t.Fatalf("sigstorePreflight: %v", err)
	}
	sig, cert, ok := cache.lookup("branchdam-agent-linux-amd64.tar.gz")
	if !ok {
		t.Fatal("cache.lookup did not find the entry")
	}
	if string(sig) != sigBody {
		t.Errorf("sig = %q, want %q", sig, sigBody)
	}
	if string(cert) != certBody {
		t.Errorf("cert = %q, want %q", cert, certBody)
	}
}

// TestSigstorePreflightMissingAttestation asserts the 404 path
// returns ErrSigstoreAttestationMissing, NOT a generic HTTP error.
func TestSigstorePreflightMissingAttestation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	assetURL := srv.URL + "/releases/v1.2.3/missing.tar.gz"
	cache := newAttestCache()
	err := sigstorePreflight(context.Background(), []assetAttestation{
		{Name: "missing.tar.gz", URL: assetURL},
	}, cache)
	if !errors.Is(err, ErrSigstoreAttestationMissing) {
		t.Errorf("err = %v, want errors.Is ErrSigstoreAttestationMissing", err)
	}
}

// TestSigstorePreflightDeduplicatesByName asserts the dedupe path:
// passing the same asset twice (the Windows case) makes only one
// network round trip per file.
func TestSigstorePreflightDeduplicatesByName(t *testing.T) {
	var sigHits, certHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.2.3/asset.tar.gz.sig":
			sigHits++
			_, _ = w.Write([]byte("sig"))
		case "/v1.2.3/asset.tar.gz.cert":
			certHits++
			_, _ = w.Write([]byte("cert"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	assetURL := srv.URL + "/v1.2.3/asset.tar.gz"
	cache := newAttestCache()
	assets := []assetAttestation{
		{Name: "asset.tar.gz", URL: assetURL},
		{Name: "asset.tar.gz", URL: assetURL}, // duplicate, e.g. sibling + primary
		{Name: "asset.tar.gz", URL: assetURL},
	}
	if err := sigstorePreflight(context.Background(), assets, cache); err != nil {
		t.Fatalf("sigstorePreflight: %v", err)
	}
	if sigHits != 1 {
		t.Errorf("sigHits = %d, want 1", sigHits)
	}
	if certHits != 1 {
		t.Errorf("certHits = %d, want 1", certHits)
	}
}

// TestSigstoreValidatorCompose is a direct unit test of the
// composed sigstoreValidator.Validate method. It exercises the
// three behaviors of the compose: (a) SHA256 first, (b) Sigstore
// verify second, (c) "no preflighted attestation" programming-error
// path. The per-target UpdateTo loop in Apply calls this method
// for every target, so a regression here is a regression in Apply.
func TestSigstoreValidatorCompose(t *testing.T) {
	const filename = "branchdam-agent-linux-amd64.tar.gz"

	// Build a SHA256SUMS.txt whose entry matches the test archive.
	archive := []byte("the archive bytes for the compose test")
	digest := sha256.Sum256(archive)
	sums := []byte(fmt.Sprintf("%x  %s\n", digest, filename))

	t.Run("happy path: SHA256 + Sigstore both pass", func(t *testing.T) {
		// For this subtest we need a leaf cert whose SAN and OIDC
		// issuer satisfy the PRODUCTION sigstoreTrustedIdentity,
		// because sigstoreValidator calls sigstoreVerify (which uses
		// the production identity). The production identity is:
		//   - OIDC issuer: https://token.actions.githubusercontent.com
		//   - SAN regex:   ^https://github\.com/s3ntin3l8/branchdam-agent/\.github/workflows/release-binaries\.yml@
		// so the test leaf needs both. Sigstore-go still accepts the
		// deprecated Fulcio v1 OIDC issuer extension (OID 1.3.6.1.4.1.57264.1.1)
		// which carries the OIDC URL as raw bytes, no DER encoding
		// needed.
		ca, caPriv := testCAKeypair(t, "compose test CA")
		leaf, leafPriv := signLeafWithProductionIdentity(t, ca, caPriv, "compose test leaf")
		sig := signECDSAP256SHA256(t, leafPriv, archive)

		cache := newAttestCache()
		cache.put(filename, encodeSigBase64(t, sig), encodeCertPEM(t, leaf))
		withTestTrustedRoot(t, ca)

		v := &sigstoreValidator{cache: cache}
		if err := v.Validate(filename, archive, sums); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("SHA256 mismatch aborts before Sigstore", func(t *testing.T) {
		// Cache is populated with a valid (sig, cert) pair, but the
		// validationData (SHA256SUMS.txt) does NOT match the archive
		// bytes. The SHA256 check must run first and abort; the
		// Sigstore check must NOT be reached.
		ca, caPriv := testCAKeypair(t, "compose mismatch CA")
		leaf, leafPriv := signLeafWithCA(t, ca, caPriv, "compose mismatch leaf")
		sig := signECDSAP256SHA256(t, leafPriv, archive)
		wrongSums := []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("totally different")), filename))

		cache := newAttestCache()
		cache.put(filename, encodeSigBase64(t, sig), encodeCertPEM(t, leaf))
		withTestTrustedRoot(t, ca)

		v := &sigstoreValidator{cache: cache}
		err := v.Validate(filename, archive, wrongSums)
		if err == nil {
			t.Fatal("Validate accepted a SHA256 mismatch; want error")
		}
		// Should be a SHA256 error, not a Sigstore error. The
		// go-selfupdate ChecksumValidator error has a specific
		// string; just check it's not the Sigstore sentinel.
		if errors.Is(err, ErrSigstoreVerificationFailed) {
			t.Errorf("Validate returned a Sigstore error on SHA256 mismatch; SHA256 check should have aborted first: %v", err)
		}
	})

	t.Run("missing cache entry returns ErrSigstoreAttestationMissing", func(t *testing.T) {
		// Cache is empty. The per-target Validator was called
		// without a corresponding preflight -- a programming error.
		cache := newAttestCache()
		// Intentionally no cache.put for `filename`.

		v := &sigstoreValidator{cache: cache}
		err := v.Validate(filename, archive, sums)
		if !errors.Is(err, ErrSigstoreAttestationMissing) {
			t.Errorf("err = %v, want errors.Is ErrSigstoreAttestationMissing", err)
		}
	})

	t.Run("GetValidationAssetName returns ChecksumAsset", func(t *testing.T) {
		// go-selfupdate's Validator interface requires this method
		// even though the per-target UpdateTo in Apply never calls
		// DetectLatest. Pin the contract so a future refactor
		// doesn't return something that surprises go-selfupdate.
		v := &sigstoreValidator{cache: newAttestCache()}
		if got, want := v.GetValidationAssetName("anything"), ChecksumAsset; got != want {
			t.Errorf("GetValidationAssetName = %q, want %q", got, want)
		}
	})
}

// fulcioV1IssuerExtensionOID is the deprecated Fulcio v1 OIDC
// issuer extension OID. Sigstore-go's ParseExtensions reads the
// raw bytes verbatim into Extensions.Issuer (no DER decoding),
// which is the easier test-fixture path. The v2 OID would require
// DER-encoded UTF8String.
var fulcioV1IssuerExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

// signLeafWithProductionIdentity is like signLeafWithCA but adds
// the SAN and OIDC issuer extension that production Fulcio-issued
// certs carry -- required to pass the production
// sigstoreTrustedIdentity policy used by sigstoreVerify.
func signLeafWithProductionIdentity(t *testing.T, ca *x509.Certificate, caPriv *ecdsa.PrivateKey, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	leafPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerExt := pkix.Extension{
		Id:    fulcioV1IssuerExtensionOID,
		Value: []byte("https://token.actions.githubusercontent.com"),
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs: []*url.URL{
			{Scheme: "https", Host: "github.com", Path: "/s3ntin3l8/branchdam-agent/.github/workflows/release-binaries.yml@refs/tags/v1.0.0"},
		},
		ExtraExtensions: []pkix.Extension{issuerExt},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, leafPriv
}
