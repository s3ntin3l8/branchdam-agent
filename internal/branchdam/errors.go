package branchdam

import (
	"errors"
	"fmt"
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

// Classification is the retry disposition the branchdam server intends for
// an error, per the plan's "fatal vs. transient" conformance section. This
// is advisory on the client -- the server enforces its own retry policy
// asynchronously in the drainer, invisibly to the agent (plan gap 3) -- but
// a client that already knows an error is fatal should not burn its own
// retry budget resending it.
type Classification int

const (
	// ClassificationUnknown is returned when the client cannot tell from an
	// HTTP-transport error alone (e.g. a plain network failure) whether the
	// underlying condition is fatal or transient. Treat as transient/retryable
	// unless proven otherwise -- see IsRetryable.
	ClassificationUnknown Classification = iota
	ClassificationFatal
	ClassificationTransient
)

// fatalSubstrings and transientSubstrings match the server's own
// error-message vocabulary (internal/agent/types.go's sentinel errors and
// internal/agent/drainer.go's isFatal set) closely enough to classify an
// error string surfaced in an HTTP response body. This is best-effort
// string matching, not a wire-level error code -- branchDAM has no
// structured error taxonomy on these routes (plain-text bodies for auth
// failures, Huma's generic error shape otherwise) -- so a change to the
// server's wording can silently defeat this; it exists to avoid burning a
// retry budget on an error the server documents as permanent, not to be a
// source of truth.
var fatalSubstrings = []string{
	"malformed event payload",
	"unknown event type",
	"invalid or empty node_uuid",
	"rebase target resolves to read-only tier",
	"node is archived, refusing to rebase",
	"edge would create a cycle",
	"constraint failed",
}

var transientSubstrings = []string{
	"media node not found",
	"rebase target resolves to tier 3 but the file does not exist there yet",
}

// ClassifyError applies fatalSubstrings/transientSubstrings to msg (a
// lowercased error message, typically an HTTP response body) and returns the
// resulting Classification. See the fatal-vs-transient table in the plan's
// conformance contract: fatal errors get zero retries (malformed payload,
// unknown event type, invalid node UUID, read-only non-Tier-3 rebase,
// ARCHIVED node, would-create-cycle, and any error containing "constraint
// failed"); transient errors get the normal retry/backoff budget
// (node-not-found, ErrArchiveFileNotYetPresent).
func ClassifyError(msg string) Classification {
	lower := strings.ToLower(msg)
	for _, s := range fatalSubstrings {
		if strings.Contains(lower, s) {
			return ClassificationFatal
		}
	}
	for _, s := range transientSubstrings {
		if strings.Contains(lower, s) {
			return ClassificationTransient
		}
	}
	return ClassificationUnknown
}

// HTTPError is returned by client methods for any non-2xx response.
// 401/503 bodies from the auth middleware are plain text ("invalid or
// missing X-API-Key", "agent authentication is not configured"), not JSON --
// Body is always the raw response body, never assumed to be a parsed error
// struct.
//
// Body is preserved as a field so HTTPError.Classification() can apply
// the fatalSubstrings/transientSubstrings rule, but Error() deliberately
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

// Retryable reports whether the HTTP status code alone suggests a retry is
// worthwhile: request timeouts, 429, and 5xx. A 401/403/503 auth failure is
// NOT retryable by a fixed backoff -- 401/403 mean the key itself is wrong,
// and 503 means the server's key is misconfigured (under 32 chars); no
// amount of retrying fixes either without an operator change. 4xx other
// than 429 is a client-side bug (bad payload shape), also not retryable.
func (e *HTTPError) Retryable() bool {
	switch {
	case e.StatusCode == 401 || e.StatusCode == 403 || e.StatusCode == 503:
		return false
	case e.StatusCode == 429:
		return true
	case e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

// Classification derives a Classification from the HTTP status and body,
// applying ClassifyError to the body text as a best-effort refinement over
// the coarse Retryable() signal above.
func (e *HTTPError) Classification() Classification {
	if c := ClassifyError(e.Body); c != ClassificationUnknown {
		return c
	}
	if e.Retryable() {
		return ClassificationTransient
	}
	return ClassificationFatal
}
