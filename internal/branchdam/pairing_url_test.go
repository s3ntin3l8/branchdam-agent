package branchdam

import (
	"errors"
	"strings"
	"testing"
)

func TestParsePairingURL(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		want      PairingURL
		wantErr   bool
		errSubstr string
	}{
		{
			name:  "canonical query-style",
			input: "branchdam://?server=https%3A%2F%2Fdam.example.com%3A8443&key=secret-key-xyz&agent=iphone-abc",
			want: PairingURL{
				Server: "https://dam.example.com:8443",
				Key:    "secret-key-xyz",
				Agent:  "iphone-abc",
			},
		},
		{
			name:  "with port",
			input: "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev-d2610219",
			want: PairingURL{
				Server: "https://dam.example.com",
				Key:    "k1",
				Agent:  "dev-d2610219",
			},
		},
		{
			name:  "reserved characters round-trip",
			input: "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k%26y%3Dwith%2Bspecials&agent=Bj%C3%B6rn%27s%20iPhone",
			want: PairingURL{
				Server: "https://dam.example.com",
				Key:    "k&y=with+specials",
				Agent:  "Björn's iPhone",
			},
		},
		{
			name:  "legacy path-style (mobile QrParser compatibility)",
			input: "branchdam://server=https%3A%2F%2Fdam.example.com/key=k1/agent=dev-d2610219",
			want: PairingURL{
				Server: "https://dam.example.com",
				Key:    "k1",
				Agent:  "dev-d2610219",
			},
		},
		{
			name:      "wrong scheme",
			input:     "https://example.com/?server=x&key=y&agent=z",
			wantErr:   true,
			errSubstr: "scheme must be",
		},
		{
			name:      "missing agent",
			input:     "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1",
			wantErr:   true,
			errSubstr: "missing required field",
		},
		{
			name:      "missing key",
			input:     "branchdam://?server=https%3A%2F%2Fdam.example.com&agent=dev-x",
			wantErr:   true,
			errSubstr: "missing required field",
		},
		{
			name:      "missing server",
			input:     "branchdam://?key=k1&agent=dev-x",
			wantErr:   true,
			errSubstr: "missing required field",
		},
		{
			name:      "empty body",
			input:     "branchdam://",
			wantErr:   true,
			errSubstr: "query-style expected",
		},
		{
			name:      "garbage body",
			input:     "branchdam://not-a-url",
			wantErr:   true,
			errSubstr: "query-style expected",
		},
		{
			// Scheme matches (the URL starts with branchdam://), but the
			// post-scheme body is CRLF + garbage. The error message must
			// not echo the body at all -- post-Hermes-review, safePrefix
			// replaces any non-query post-scheme body with <elided> (the
			// CR/LF can't smuggle through, and neither can the garbage
			// that follows). Stricter than just sanitizeForLog's CRLF
			// escape; a forged log line is structurally impossible in
			// this error path.
			name:      "CRLF injection in error path (CWE-117)",
			input:     "branchdam://\r\nfake-log-line",
			wantErr:   true,
			errSubstr: `<elided>`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePairingURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want error containing %q", tc.errSubstr)
				}
				if !errors.Is(err, ErrPairingURLInvalid) {
					t.Errorf("err = %v, want errors.Is(err, ErrPairingURLInvalid)", err)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("err = %q, want substring %q", err.Error(), tc.errSubstr)
				}
				// Defense-in-depth: the error message must never echo the
				// plaintext key back. Errors here are user-facing too --
				// logged at startup if a paste was malformed -- so a
				// regression that leaked the key into the error string
				// would put it in every operator's logs.
				if got.Key != "" {
					t.Errorf("err path leaked key: %q", got.Key)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestParsePairingURLErrorRedactsKey verifies that a malformed URL with
// a key-shaped value still has its key elided in the resulting error
// message. Without redactKey, a typo'd paste would log the plaintext
// API key into operator.log on every `branchdam-agent pair` retry.
func TestParsePairingURLErrorRedactsKey(t *testing.T) {
	const plaintext = "this-could-be-a-real-key-dont-leak"
	// Missing agent -> error path; key is present in the URL.
	_, err := ParsePairingURL("branchdam://?server=https://x&key=" + plaintext)
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Errorf("error message leaked plaintext key: %v", err)
	}
	if !strings.Contains(err.Error(), "<elided>") {
		t.Errorf("error message should redact key as <elided>, got: %v", err)
	}
}

// TestParsePairingURLErrorRedactsKeyInEchoedPrefix guards against the
// regression called out in PR #243's Hermes review: a URL with a
// typo'd scheme (e.g. branchdm://) would otherwise echo the first 40
// chars of the raw URL into the error message, and if that prefix
// happened to span `key=<plaintext>`, the plaintext would land in
// operator.log on every malformed-paste retry. safePrefix now applies
// redactKeyInRaw before embedding the prefix; this test pins that.
func TestParsePairingURLErrorRedactsKeyInEchoedPrefix(t *testing.T) {
	const plaintext = "0123456789abcdef0123456789abcdef" // exactly 32 chars, fits in safePrefix's 40-char window
	cases := []struct {
		name  string
		input string
	}{
		{
			"typo'd scheme, key within echoed prefix",
			"branchdm://?server=https://x&key=" + plaintext + "&agent=dev-x",
		},
		{
			"correct scheme, key within echoed prefix at query-style-expected branch",
			"branchdam://" + plaintext + "&agent=dev-x", // no leading "?", triggers query-style-expected error path
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePairingURL(tc.input)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), plaintext) {
				t.Errorf("error message leaked plaintext key: %v", err)
			}
			if !strings.Contains(err.Error(), "<elided>") {
				t.Errorf("error message should redact key as <elided>, got: %v", err)
			}
		})
	}
}

// TestParsePairingURLRejectsCleartextServerOnNonLoopbackHost verifies
// the server-URL validation guard added in PR #243's Hermes review:
// a malformed or cleartext-non-loopback server field used to slip
// through to client.New and panic via validateBaseURL, leaving a Go
// stack trace instead of the typed ErrPairingURLInvalid every other
// branch produces. Now ParsePairingURL catches it before the panic
// path can fire.
func TestParsePairingURLRejectsCleartextServerOnNonLoopbackHost(t *testing.T) {
	cases := []struct {
		name      string
		server    string
		errSubstr string
	}{
		{
			name:      "cleartext http on non-loopback",
			server:    "http://dam.example.com",
			errSubstr: "cleartext http",
		},
		{
			name:      "unsupported scheme",
			server:    "ftp://dam.example.com",
			errSubstr: "scheme",
		},
		{
			name:      "unparseable URL",
			server:    "://garbage",
			errSubstr: "not a valid URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePairingURL("branchdam://?server=" + urlEncodeForTest(tc.server) + "&key=k1&agent=dev-x")
			if err == nil {
				t.Fatal("expected error for bad server")
			}
			if !errors.Is(err, ErrPairingURLInvalid) {
				t.Errorf("err = %v, want errors.Is(err, ErrPairingURLInvalid)", err)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.errSubstr)
			}
		})
	}
}

// TestParsePairingURLServerShape pins the scheme://host[:port]-only shape
// gate (no userinfo, path, query, or fragment in the server field). The
// userinfo case is the security-critical one: "https://attacker@trusted"
// would pair the agent against the userinfo host, not the host the
// operator believes they paired with. The path/query cases matter too:
// client.go concatenates baseURL+path verbatim, so a server field with a
// path would produce "host/path/api/..." requests and a config.yaml that
// Load accepts but every request mangles.
func TestParsePairingURLServerShape(t *testing.T) {
	rejects := []struct {
		name      string
		server    string
		errSubstr string
	}{
		{"userinfo", "https://user:pass@dam.example.com", "scheme://host[:port]"}, // pragma: allowlist secret -- "user:pass" is a fixture, not a credential
		{"userinfo without password", "https://attacker@dam.example.com", "scheme://host[:port]"},
		{"path", "https://dam.example.com/branchdam", "scheme://host[:port]"},
		{"path with trailing slash", "https://dam.example.com/", "scheme://host[:port]"},
		{"query", "https://dam.example.com?x=1", "scheme://host[:port]"},
		{"fragment", "https://dam.example.com#frag", "scheme://host[:port]"},
		// Rejected one gate earlier (ValidateServerURL's cleartext
		// rule), not by the shape regexp -- same typed error, different
		// message.
		{"non-loopback http with port", "http://192.168.1.1:8080", "cleartext http"},
	}
	for _, tc := range rejects {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			_, err := ParsePairingURL("branchdam://?server=" + urlEncodeForTest(tc.server) + "&key=k1&agent=dev-x")
			if err == nil {
				t.Fatalf("server %q: err = nil, want rejection", tc.server)
			}
			if !errors.Is(err, ErrPairingURLInvalid) {
				t.Errorf("err = %v, want errors.Is(err, ErrPairingURLInvalid)", err)
			}
			if !strings.Contains(err.Error(), tc.errSubstr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.errSubstr)
			}
		})
	}

	accepts := []string{
		"https://dam.example.com",
		"https://dam.example.com:8443",
		"https://sub.dam-example.com",
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"http://[::1]:9000",
		// Everything below is host policy, not shape: ValidateServerURL's
		// isLoopbackHost accepts all of 127.0.0.0/8, expanded IPv6
		// loopback, and bracketed IPv6 on either scheme -- the shape
		// gate must not narrow that policy (Hermes round-2 review,
		// PR #261).
		"http://127.0.0.2:8080",
		"http://[0:0:0:0:0:0:0:1]:9000",
		"https://[::1]:8443",
		"https://[2001:db8::1]:8443",
		"https://dam_example.com",
		// url.Parse lowercases the scheme, so ValidateServerURL accepts
		// an uppercase-scheme URL -- the shape gate must not reject it
		// (would be a policy divergence in the opposite direction).
		"HTTPS://dam.example.com",
	}
	for _, server := range accepts {
		t.Run("accept/"+server, func(t *testing.T) {
			got, err := ParsePairingURL("branchdam://?server=" + urlEncodeForTest(server) + "&key=k1&agent=dev-x")
			if err != nil {
				t.Fatalf("server %q: err = %v, want nil", server, err)
			}
			if got.Server != server {
				t.Errorf("got.Server = %q, want %q", got.Server, server)
			}
		})
	}
}

// TestValidateServerURL covers the exported server-URL gate now shared
// between client.New and ParsePairingURL. The two callers' contracts
// diverge (New panics on failure; ParsePairingURL wraps), but both
// must agree on what counts as a valid URL.
func TestValidateServerURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https on public host", "https://dam.example.com", false},
		{"https with port", "https://dam.example.com:8443", false},
		{"http on loopback IPv4", "http://127.0.0.1:8080", false},
		{"http on loopback hostname", "http://localhost:8080", false},
		{"http on non-loopback", "http://dam.example.com", true},
		{"unsupported scheme", "ftp://dam.example.com", true},
		{"empty scheme", "dam.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServerURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateServerURL(%q) = nil, want error", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateServerURL(%q) = %v, want nil", tc.url, err)
			}
		})
	}
}

// urlEncodeForTest percent-encodes s the same way ParsePairingURL's
// server field would arrive from a real QR payload. Kept local to the
// test file so the parser's tests don't import net/url just for one
// helper.
func urlEncodeForTest(s string) string {
	const hexChars = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexChars[c>>4])
		b.WriteByte(hexChars[c&0x0F])
	}
	return b.String()
}

// The deep-link confirmation dialog shows server and agent verbatim, so
// neither may forge lines or rewrite the host visually.
func TestParsePairingURLRejectsUndisplayableFields(t *testing.T) {
	longAgent := strings.Repeat("a", maxAgentRunes+1)
	cases := map[string]string{
		"newline in agent":    "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev-x%0AServer%3A%20https%3A%2F%2Fevil.example",
		"bidi override host":  "branchdam://?server=https%3A%2F%2Fdam.example.com%E2%80%AE&key=k1&agent=dev-x",
		"zero-width agent":    "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev%E2%80%8Bx",
		"line separator":      "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev%E2%80%A8x",
		"paragraph separator": "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev%E2%80%A9x",
		"tab in agent":        "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=dev%09x",
		"over-long agent":     "branchdam://?server=https%3A%2F%2Fdam.example.com&key=k1&agent=" + longAgent,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePairingURL(in)
			if !errors.Is(err, ErrPairingURLInvalid) {
				t.Fatalf("err = %v, want ErrPairingURLInvalid", err)
			}
		})
	}
}
