package branchdam

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrPairingURLInvalid is returned by ParsePairingURL when the input is
// not a recognized branchdam:// pairing URL (wrong scheme, missing
// required field, or unparseable query). The caller can errors.Is against
// it for a single branch; the error message carries the specific reason
// for human display.
var ErrPairingURLInvalid = errors.New("pairing url invalid")

// ErrPairingConfigInvalid is returned (wrapped) when the credentials from
// an otherwise well-formed pairing URL fail pre-write config validation
// (e.g. the server field trips a config.Load invariant such as the
// trailing-slash rule). It marks the failure as CLIENT-side: the paste is
// bad, the file was not touched. Callers distinguishing "your input was
// rejected" (HTTP 400) from "the server failed" (HTTP 500) should
// errors.Is against this sentinel -- NOT match message text, since the
// post-patch reload path produces a bare "config problem: ..." error that
// is a genuine server-side 500 even though the text is identical.
var ErrPairingConfigInvalid = errors.New("pairing config invalid")

// ErrPairingAgentBlank is returned when the pairing URL's agent field is
// empty or whitespace-only after trimming. Client-side failure --
// errors.Is against it for 400-class mapping.
var ErrPairingAgentBlank = errors.New("agentId cannot be blank")

// ErrPairingHelloFailed marks a failure of the pre-write Hello()
// credential validation against the proposed server -- whether the server
// replied with an error (*HTTPError, reachable via errors.As) or never
// replied at all (dial/DNS/timeout). Both are client-input conditions from
// the pairing flow's perspective (mistyped URL, dead host, revoked key), so
// callers mapping to HTTP status should report 400, not 500.
var ErrPairingHelloFailed = errors.New("pairing hello validation failed")

// PairingURL is the parsed form of a branchdam:// pairing URL emitted by
// branchDAM's Companion Pairing endpoint (see branchdam-server's
// internal/httpapi.qrPayloadFor and the round-trip test
// TestCompanionPairings_QRPayloadRoundTripsViaStandardQueryParser).
//
// The wire format is:
//
//	branchdam://?server=<URL>&key=<KEY>&agent=<AGENT_ID>
//
// All three fields are required. server is the server's externally
// reachable URL (already extracted from X-Forwarded-* by the server),
// key is the plaintext API key (the QR shows this exactly once), and
// agent is the server-minted agent_id ("dev-xxxxxxxx"). The workstation
// agent uses agent as its body.AgentID on every authenticated request,
// replacing the self-asserted id its resolve/sync.go fallback used to
// mint -- the server's /agent/handshake cross-check at
// internal/httpapi/routes.go requires body.AgentID to match the
// Principal.Name, and Principal.Name is the paired agent_id LookupKey
// resolves to.
type PairingURL struct {
	// Server is the externally reachable server URL (scheme + host [+ port]).
	Server string
	// Key is the plaintext API key. Treated as a secret -- callers must
	// not log it.
	Key string
	// Agent is the server-minted agent_id (e.g. "dev-d2610219"). Used as
	// the workstation agent's AgentID config field; replaces the
	// self-asserted fallback in internal/resolve/sync.go's resolveAgentID.
	Agent string
}

// ParsePairingURL parses a branchdam:// pairing URL into its three
// required fields. Returns ErrPairingURLInvalid wrapping a specific
// reason for any failure (wrong scheme, missing field, unparseable
// query). The mobile companion app's QrParser accepts both the
// query-style form (branchdam://?server=...&key=...&agent=...) and the
// older fragment-style form (branchdam://server=.../key=.../agent=...);
// ParsePairingURL accepts both for the same reason -- a workstation
// operator who has an older QR code lying around shouldn't have to
// re-pair.
//
// Both server and key must be non-empty; agent must be non-empty too
// because the workstation agent uses it as its AgentID config field
// and a missing agent_id would fail the server's handshake cross-check.
// The server's qrPayloadFor never emits an empty field, so an empty
// value here is always a malformed URL.
func ParsePairingURL(rawURL string) (PairingURL, error) {
	const scheme = "branchdam://"

	if !strings.HasPrefix(rawURL, scheme) {
		return PairingURL{}, fmt.Errorf("%w: scheme must be %s (got prefix %s)", ErrPairingURLInvalid, scheme, safePrefix(rawURL))
	}

	// Strip the scheme. The remainder is either "?...query" (query-style,
	// what the server emits today) or a path-style legacy form. Parse as
	// a URL first; if that fails (because the input is just "key/value"
	// without the "?") fall back to splitting on "/" to honor the legacy
	// fragment-style form the mobile app still accepts.
	body := strings.TrimPrefix(rawURL, scheme)
	var values url.Values
	if strings.HasPrefix(body, "?") {
		v, err := url.ParseQuery(strings.TrimPrefix(body, "?"))
		if err != nil {
			return PairingURL{}, fmt.Errorf("%w: %v", ErrPairingURLInvalid, err)
		}
		values = v
	} else {
		values = parseLegacyPairingURL(body)
		if values == nil {
			return PairingURL{}, fmt.Errorf("%w: query-style expected %s, got %s", ErrPairingURLInvalid, scheme+"?<query>", safePrefix(rawURL))
		}
	}

	server := values.Get("server")
	key := values.Get("key")
	agent := values.Get("agent")
	if server == "" || key == "" || agent == "" {
		return PairingURL{}, fmt.Errorf("%w: missing required field (server=%q key=%q agent=%q)", ErrPairingURLInvalid, server, redactKey(key), agent)
	}

	// server and agent are shown verbatim in the deep-link confirmation
	// dialog, the only control on a web-page-fired link, so they must not be
	// able to forge lines (newlines) or visually rewrite the host (bidi
	// overrides, zero-width characters). Neither appears in a real pairing.
	if err := checkDisplayable("server", server, maxServerRunes); err != nil {
		return PairingURL{}, err
	}
	if err := checkDisplayable("agent", agent, maxAgentRunes); err != nil {
		return PairingURL{}, err
	}

	// Validate the server URL the same way client.New's constructor
	// would: bad scheme, cleartext on a non-loopback host, or otherwise
	// unparseable. Without this, a malformed server field would slip
	// through to client.New and panic via validateBaseURL, leaving a
	// Go-stack-trace error instead of the typed ErrPairingURLInvalid
	// every other branch of this function deliberately produces. The
	// pairing-URL parser is a user-input gate -- New is a programmer-
	// input gate -- and the same allow/deny rule must apply at both
	// without forcing a panic at the user-input gate.
	if err := ValidateServerURL(server); err != nil {
		return PairingURL{}, fmt.Errorf("%w: %v", ErrPairingURLInvalid, err)
	}

	// Shape gate: the server field must be exactly scheme://authority --
	// no userinfo, path, query, or fragment. This regexp deliberately
	// encodes ONLY that structural invariant. Scheme and host policy
	// (http/https allowlist, cleartext-non-loopback refusal, all of
	// 127.0.0.0/8, expanded or bracketed IPv6 loopback) stays solely in
	// ValidateServerURL/isLoopbackHost, which ran immediately above --
	// one authoritative policy, not two diverging copies. The structural
	// gate is still a real tightening, not lint appeasement:
	//
	//   - a userinfo component (https://<user>:<pass>@host) would send the
	//     operator's pairing request (and later every signed agent request)
	//     to the userinfo host, not the host the operator believes they
	//     paired with -- a pasted-URL phishing vector;
	//   - a path/query/fragment would flow verbatim into client.go's
	//     baseURL+path concatenation, producing "host/path/api/..."
	//     requests and a config.yaml that fails in non-obvious ways.
	//
	// Implementation note: this MUST stay a regexp-match used as an
	// if-condition barrier over `server`. CodeQL's go/request-forgery
	// (CWE-918) query recognizes regexp-match barrier guards as
	// sanitizers, and this guard severs the taint path from the pairing
	// URL to http.NewRequest in Client.post for every consumer of
	// PairingURL.Server (pair CLI, deep-link dispatch, the
	// /api/actions/pair loopback endpoint). A plain helper call here
	// (e.g. isLoopbackHost + u.User == nil checks) would NOT be
	// recognized and would reopen the alert. The security justification
	// stands on the allowlist above regardless of the analyzer.
	if !pairingServerShape.MatchString(server) {
		return PairingURL{}, fmt.Errorf("%w: server %q must be scheme://host[:port] only (no userinfo, path, query, or fragment)", ErrPairingURLInvalid, server)
	}

	return PairingURL{Server: server, Key: key, Agent: agent}, nil
}

// Length caps for the two fields the confirmation dialog displays; generous
// for real hosts/agent ids, small enough that a dialog stays readable.
const (
	maxServerRunes = 253
	maxAgentRunes  = 100
)

// IsDisplayable reports whether v is safe to interpolate into a dialog: no
// control characters (incl. CR/LF), no Unicode format characters (bidi
// overrides/isolates, zero-width joiners) and no line/paragraph separators
// (U+2028/U+2029, which Cocoa renders as line breaks).
func IsDisplayable(v string) bool {
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// checkDisplayable rejects non-displayable and over-long values. The value is
// deliberately not echoed in the error.
func checkDisplayable(field, v string, maxRunes int) error {
	if utf8.RuneCountInString(v) > maxRunes {
		return fmt.Errorf("%w: %s is longer than %d characters", ErrPairingURLInvalid, field, maxRunes)
	}
	if !IsDisplayable(v) {
		return fmt.Errorf("%w: %s contains control or invisible formatting characters", ErrPairingURLInvalid, field)
	}
	return nil
}

// pairingServerShape matches exactly scheme://authority: an absolute-URL
// prefix with NOTHING after the authority -- no userinfo ("@"), path,
// query ("?"), or fragment ("#"). Host policy is intentionally NOT encoded
// here; see the guard's comment in ParsePairingURL. The scheme class is
// case-insensitive because url.Parse normalizes the scheme to lowercase,
// so ValidateServerURL above already accepted "HTTPS://..." by the time
// this gate runs -- rejecting it here would reintroduce a policy divergence
// in the opposite direction.
var pairingServerShape = regexp.MustCompile(`^(?i:[a-z][a-z0-9+.-]*)://[^/?#@]+$`)

// parseLegacyPairingURL parses the older "key/value/key/value" fragment
// form. Returns nil if no recognizable key appears; the caller then
// surfaces ErrPairingURLInvalid with the specific reason.
//
// The mobile app's old QrParser documents the legacy form as a
// "/"-separated path with each segment "field=value" (see
// branchdam-server's docs/mobile.md §4.5), so we split on "/" and
// match each "field=value" by the leading field name. This is best-
// effort -- the canonical form is query-style and any new operator
// onboarding today will be using the query form anyway.
//
// Known truncation: a key value containing an unencoded "/" is split
// at that slash and the trailing segment is parsed as the next field
// (and silently dropped if it's not `agent=...`). The Companion
// Pairing mint path (internal/pairing/service.go's mintAPIKey) uses
// base64.URLEncoding without padding, so a generated key never
// contains "/"; only operator-typed values can. Since the legacy form
// is supported solely for backward compatibility with already-issued
// QR codes, and the canonical query form is what new QR codes use,
// accepting this best-effort behavior keeps the parser simple rather
// than adding a "did the server actually emit this?" disambiguation.
func parseLegacyPairingURL(body string) url.Values {
	if body == "" {
		return nil
	}
	v := url.Values{}
	for _, seg := range strings.Split(body, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		eq := strings.IndexByte(seg, '=')
		if eq <= 0 {
			continue
		}
		k := seg[:eq]
		val := seg[eq+1:]
		// url.PathUnescape so the legacy form supports percent-encoding
		// for spaces and reserved chars the way the query form does.
		if unesc, err := url.PathUnescape(val); err == nil {
			val = unesc
		}
		v.Set(k, val)
	}
	if v.Get("server") == "" && v.Get("key") == "" && v.Get("agent") == "" {
		return nil
	}
	return v
}

// safePrefix returns the first ~40 chars of s for inclusion in an error
// message, replacing any CR/LF with their visible escape (so a
// maliciously-crafted URL can't smuggle a forged log line, CWE-117) AND
// redacting any `key=<value>` segment so a typo'd scheme (e.g.
// `branchdm://`) doesn't echo the plaintext API key into stderr /
// operator.log. Mirrors branchdam-server's sanitizeForLog posture
// (CR/LF) plus the redactKey posture the rest of this file already
// applies to parsed values.
//
// If s starts with branchdam:// but the post-scheme body is not a
// `?...` query (i.e. the legacy-fragment form, or just garbage), the
// entire body is treated as potentially secret and replaced with
// <elided> -- covers the case where an operator pastes a bare key
// value (no key= label) after the scheme, which the parser would
// otherwise echo verbatim into the error message.
func safePrefix(s string) string {
	// 64 (rather than the previous 40) so the full <elided> marker
	// survives a worst-case truncation -- e.g. an input like
	// `branchdm://?server=example.com&key=<plaintext>` truncates
	// cleanly to `...key=<elided>` instead of cutting mid-token.
	const max = 64
	s = redactKeyInRaw(s)
	if strings.HasPrefix(s, "branchdam://") {
		body := strings.TrimPrefix(s, "branchdam://")
		if !strings.HasPrefix(body, "?") {
			s = "branchdam://" + "<elided>"
		}
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// redactKeyInRaw replaces any `key=<value>` segment in a raw URL string
// with `key=<elided>`. The match stops at the next `&` (query-style
// boundary) or end-of-string; both forms the server and the legacy
// fragment form use the same `key=` field name, so a single regex-free
// scan covers them. Empty `key=` (followed immediately by `&` or EOF)
// passes through -- the empty-key case is already covered by the
// missing-field branch in ParsePairingURL, so an empty value here
// doesn't carry secret material.
func redactKeyInRaw(s string) string {
	const marker = "key="
	idx := strings.Index(s, marker)
	if idx < 0 {
		return s
	}
	start := idx + len(marker)
	end := start
	for end < len(s) && s[end] != '&' {
		end++
	}
	return s[:start] + "<elided>" + s[end:]
}

// redactKey returns "<elided>" for any non-empty key, so error messages
// produced during parsing don't leak the plaintext API key into logs.
// Empty keys pass through unchanged so the error message can still say
// "key=".
func redactKey(k string) string {
	if k == "" {
		return k
	}
	return "<elided>"
}
