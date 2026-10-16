package branchdam

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel validation errors, checked client-side before a request is ever
// sent -- the server does not validate an EVENT_EDGE_ATTACHED payload at
// enqueue time (202 regardless), so this package is the only place these
// can be caught before they burn retries and land FAILED with no feedback
// channel (plan gap 3).
var (
	ErrConfidenceOutOfRange  = errors.New("branchdam: confidence must be in (0,1] and >= 0.50")
	ErrInvalidTier           = errors.New("branchdam: tier must be 1, 2, or 3")
	ErrInvalidRelationship   = errors.New("branchdam: relationshipType is not one of the five accepted values")
	ErrReviewStateNotAllowed = errors.New("branchdam: reviewState must never be set by an agent")
)

// validTiers/validRelationships back ValidateEdgeAttached.
var validTiers = map[int64]bool{1: true, 2: true, 3: true}

var validRelationships = map[string]bool{
	RelationshipDerivedFrom:    true,
	RelationshipFinalExport:    true,
	RelationshipProxyOf:        true,
	RelationshipProjectSidecar: true,
	RelationshipDuplicateOf:    true,
}

// minConfidence matches the server's hard gate on EdgeAttachedPayload.Confidence.
const minConfidence = 0.50

// ValidateEdgeAttached checks the hard gates branchDAM's server enforces on
// EVENT_EDGE_ATTACHED, so a caller catches a bad payload before POSTing it
// rather than after: confidence must be in (0,1] AND >= 0.50 (a value
// outside that range is fatal server-side, zero retries), tier must be in
// {1,2,3}, relationshipType must be one of the five accepted values (not
// validated in Go server-side -- an unrecognized value hits a SQLite CHECK
// and is fatal), and reviewState must never be set (CONFIRMED/REJECTED is an
// outright error -- a human review decision is never the agent's to make).
func ValidateEdgeAttached(p EdgeAttachedPayload) error {
	if p.Confidence <= 0 || p.Confidence > 1 || p.Confidence < minConfidence {
		return ErrConfidenceOutOfRange
	}
	if !validTiers[p.Tier] {
		return ErrInvalidTier
	}
	if !validRelationships[p.RelationshipType] {
		return ErrInvalidRelationship
	}
	if p.ReviewState != "" {
		return ErrReviewStateNotAllowed
	}
	return nil
}

// Classification is the retry disposition for a failed agent API call:
// whether resending the same request later can succeed (Transient) or the
// server has refused it for a reason that will not change on its own
// (Fatal). The offline drainer (internal/ingest/drain.go) marks a rebase
// row terminally FAILED only on ClassificationFatal and retries everything
// else with backoff, so the default here is deliberately Transient: an
// error wrongly called fatal strands a row until an operator resets it
// (`queue-drain -retry-failed`), while one wrongly called transient merely
// costs a request every few minutes.
type Classification int

const (
	// ClassificationUnknown is the zero value. HTTPError.Classification
	// never returns it; it remains for callers that classify a non-HTTP
	// error themselves and want to say "can't tell" -- treat it as
	// transient.
	ClassificationUnknown Classification = iota
	ClassificationFatal
	ClassificationTransient
)

// fatalCodes / transientCodes map the machine-readable `code` field of the
// server's problem+json error envelope (branchDAM's planned uniform agent
// error envelope, which keeps HTTP statuses unchanged and adds `code`) to a
// Classification. A recognized code wins over every other signal; an
// unrecognized or absent code falls through to the body hints and status
// rules below, so this package works against servers that predate the
// field and keeps working if a new code appears.
var fatalCodes = map[string]bool{
	"HASH_CONFLICT":     true, // node already has a different fullHash
	"NODE_ARCHIVED":     true, // refusing to resurrect a superseded version
	"NODE_NOT_OWNER":    true, // node was created by a different device
	"VALIDATION_FAILED": true, // request itself is malformed
	"TARGET_READ_ONLY":  true, // target resolves to a read-only, non-Tier-3 location
}

var transientCodes = map[string]bool{
	"AUTH_INVALID_KEY":             true, // fixed by re-pairing or a config change, not by dropping the row
	"AUTH_INVALID_SIGNATURE":       true,
	"AUTH_NOT_CONFIGURED":          true, // server-side key misconfiguration
	"NODE_NOT_FOUND":               true, // EVENT_NODE_CREATED not applied yet
	"ARCHIVE_FILE_NOT_YET_PRESENT": true, // NAS view on the server lags the agent's copy
	"RATE_LIMITED":                 true,
}

// transientBodyHints are messages from handleAgentRebase (branchDAM's
// internal/httpapi/routes.go) and the auth middleware that arrive as 400
// but describe a condition that clears by itself. Matched
// case-insensitively against the raw body, before the status rules, so
// each entry must be specific to its 400 source: a generic phrase here
// would override a genuine fatal refusal that happened to contain it.
var transientBodyHints = []string{
	// 400: Tier-3 target the server cannot see yet (archive mount lag).
	"no file exists there yet",
	"file does not exist there yet",
	// 400: the server's own stat of the target failed (I/O, mount down).
	"checking whether rebase target",
	// 400 from the auth middleware: the request body was cut off in transit.
	"failed to read request body",
}

// fatalBodyHints are server messages that name an explicit, permanent
// refusal arriving with a status that is otherwise transient.
var fatalBodyHints = []string{
	// 403 from handleAgentRebase's ownership check: this device's key did
	// not create the node, and retrying with the same key never will.
	"node was not created by this device",
}

// HTTPError is returned by client methods for any non-2xx response.
// 401/503 bodies from the auth middleware are plain text ("invalid or
// missing X-API-Key", "agent authentication is not configured"), not JSON --
// Body is always the raw response body, never assumed to be a parsed error
// struct.
//
// Body is preserved as a field so HTTPError.Classification() can read the
// server's error code and message, but Error() deliberately
// does NOT echo the body verbatim (audit S-7): a server that reflects
// the request payload back, or names the X-API-Key in its debug output,
// would otherwise be re-leaked into slog/crash-dump paths. The body is
// always available to callers via the Body field for structured logging
// or replay-driven debugging.
//
// IMPORTANT: callers logging this error MUST log err.Error() (the
// status-only message) rather than the *HTTPError struct via %+v or
// %#v -- both of those would dump the raw Body field verbatim,
// re-introducing the S-7 leak that Error()'s sanitization exists to
// prevent. To capture the body for diagnostic purposes, read httpErr.Body
// explicitly under a redactor-aware logger, never via the error's own
// formatted representation.
type HTTPError struct {
	StatusCode int
	Body       string
}

// Error returns a short, status-only message. The body is intentionally
// omitted -- see HTTPError's doc comment. Callers that need the body
// for logging or Classification read the Body field directly.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("branchdam: server returned HTTP %d", e.StatusCode)
}

// problemDoc is the subset of an RFC 9457 problem+json body (Huma v2's
// error model) this package reads.
type problemDoc struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

// problem parses Body as a problem document. ok is false for a plain-text
// body (the auth middleware, Go's mux, a reverse proxy) or a JSON body with
// none of the problem fields.
func (e *HTTPError) problem() (problemDoc, bool) {
	var p problemDoc
	if err := json.Unmarshal([]byte(e.Body), &p); err != nil {
		return problemDoc{}, false
	}
	return p, p.Code != "" || p.Title != "" || p.Detail != "" || p.Status != 0
}

// Code returns the machine-readable error code from a problem+json body, or
// "" when the server sent none (every server before the uniform error
// envelope, and every plain-text body).
func (e *HTTPError) Code() string {
	p, _ := e.problem()
	return p.Code
}

// Retryable reports whether resending the request later can succeed --
// i.e. Classification() is not ClassificationFatal.
func (e *HTTPError) Retryable() bool {
	return e.Classification() != ClassificationFatal
}

// Classification decides, in order:
//
//  1. a recognized `code` in a problem+json body (fatalCodes /
//     transientCodes);
//  2. transientBodyHints, then fatalBodyHints, against the raw body;
//  3. the status. Only 400, 404, 409, 410, 413 and 422 are fatal, and only
//     when the body is a problem document -- i.e. the route's own handler
//     refused the request (Huma renders every handler error as one). For
//     POST /api/v1/agent/rebase (branchDAM's handleAgentRebase) those are:
//     400 for a malformed nodeUuid/fullHash/targetPath, a target no storage
//     location covers, or a read-only non-Tier-3 target; 404 "asset not
//     found" for an ARCHIVED node (rebasing would resurrect a superseded
//     version); 409 for a fullHash that differs from the server's; 422 for
//     a schema violation. The same statuses in plain text come from the
//     auth middleware, Go's mux (a route an older server lacks) or a proxy,
//     and say nothing permanent about the request.
//
// Everything else is transient: 401/403 (key rejected or not yet accepted
// -- fixed by re-pairing or a config change, after which the queued rows
// must still go through), 408, 429, every 5xx including 503 "agent
// authentication is not configured", and any status this list does not
// name.
func (e *HTTPError) Classification() Classification {
	p, isProblem := e.problem()
	if fatalCodes[p.Code] {
		return ClassificationFatal
	}
	if transientCodes[p.Code] {
		return ClassificationTransient
	}

	lower := strings.ToLower(e.Body)
	for _, h := range transientBodyHints {
		if strings.Contains(lower, h) {
			return ClassificationTransient
		}
	}
	for _, h := range fatalBodyHints {
		if strings.Contains(lower, h) {
			return ClassificationFatal
		}
	}

	switch e.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusGone, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		if isProblem {
			return ClassificationFatal
		}
	}
	return ClassificationTransient
}
