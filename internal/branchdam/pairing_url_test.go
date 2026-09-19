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
			// sanitize the CR/LF to literal \r / \n so a forged log line
			// can't smuggle into operator.log via the error path
			// (CWE-117, mirrors branchdam-server's sanitizeForLog).
			name:      "CRLF injection in error path (CWE-117)",
			input:     "branchdam://\r\nfake-log-line",
			wantErr:   true,
			errSubstr: `\r\n`,
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
