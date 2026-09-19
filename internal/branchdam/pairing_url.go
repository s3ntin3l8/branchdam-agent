package branchdam

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrPairingURLInvalid is returned by ParsePairingURL when the input is
// not a recognized branchdam:// pairing URL (wrong scheme, missing
// required field, or unparseable query). The caller can errors.Is against
// it for a single branch; the error message carries the specific reason
// for human display.
var ErrPairingURLInvalid = errors.New("pairing url invalid")

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

	return PairingURL{Server: server, Key: key, Agent: agent}, nil
}

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

// safePrefix returns the first ~40 chars of s, replacing any CR/LF with
// their visible escape so a maliciously-crafted URL can't smuggle a
// forged log line (CWE-117). Mirrors branchdam-server's
// internal/auth.sanitizeForLog posture.
func safePrefix(s string) string {
	const max = 40
	if len(s) > max {
		s = s[:max] + "..."
	}
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
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
