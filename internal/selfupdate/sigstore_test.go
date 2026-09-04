package selfupdate

import "testing"

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
