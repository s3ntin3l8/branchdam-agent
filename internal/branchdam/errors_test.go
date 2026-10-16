package branchdam

import (
	"encoding/json"
	"testing"
)

func validEdge() EdgeAttachedPayload {
	return EdgeAttachedPayload{
		SourceNodeUUID:   "a",
		TargetNodeUUID:   "b",
		RelationshipType: RelationshipDerivedFrom,
		Confidence:       0.90,
		Tier:             1,
	}
}

func TestValidateEdgeAttachedConfidenceBounds(t *testing.T) {
	cases := []struct {
		name       string
		confidence float64
		wantErr    error
	}{
		{"exactly min (0.50) passes", 0.50, nil},
		{"below min", 0.49, ErrConfidenceOutOfRange},
		{"exactly 1.0 passes", 1.0, nil},
		{"above 1.0", 1.01, ErrConfidenceOutOfRange},
		{"zero", 0, ErrConfidenceOutOfRange},
		{"negative", -0.5, ErrConfidenceOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validEdge()
			p.Confidence = tc.confidence
			err := ValidateEdgeAttached(p)
			if err != tc.wantErr {
				t.Errorf("Confidence=%v: err = %v, want %v", tc.confidence, err, tc.wantErr)
			}
		})
	}
}

func TestValidateEdgeAttachedTier(t *testing.T) {
	for _, tier := range []int64{1, 2, 3} {
		p := validEdge()
		p.Tier = tier
		if err := ValidateEdgeAttached(p); err != nil {
			t.Errorf("tier %d: unexpected err %v", tier, err)
		}
	}
	for _, tier := range []int64{0, 4, -1} {
		p := validEdge()
		p.Tier = tier
		if err := ValidateEdgeAttached(p); err != ErrInvalidTier {
			t.Errorf("tier %d: err = %v, want ErrInvalidTier", tier, err)
		}
	}
}

func TestValidateEdgeAttachedRelationshipType(t *testing.T) {
	for _, rel := range []string{
		RelationshipDerivedFrom, RelationshipFinalExport, RelationshipProxyOf,
		RelationshipProjectSidecar, RelationshipDuplicateOf,
	} {
		p := validEdge()
		p.RelationshipType = rel
		if err := ValidateEdgeAttached(p); err != nil {
			t.Errorf("relationship %q: unexpected err %v", rel, err)
		}
	}
	for _, rel := range []string{"", "DERIVED", "final_export", "SOMETHING_ELSE"} {
		p := validEdge()
		p.RelationshipType = rel
		if err := ValidateEdgeAttached(p); err != ErrInvalidRelationship {
			t.Errorf("relationship %q: err = %v, want ErrInvalidRelationship", rel, err)
		}
	}
}

func TestValidateEdgeAttachedReviewStateRejected(t *testing.T) {
	for _, rs := range []string{"CONFIRMED", "REJECTED", "anything"} {
		p := validEdge()
		p.ReviewState = rs
		if err := ValidateEdgeAttached(p); err != ErrReviewStateNotAllowed {
			t.Errorf("reviewState %q: err = %v, want ErrReviewStateNotAllowed", rs, err)
		}
	}
}

// TestValidateEdgeAttachedChecksInOrder pins the check order: confidence is
// validated before tier, before relationship, before reviewState. A payload
// that's wrong in more than one dimension should always fail on the first
// one checked -- callers reading the error type to decide what to fix
// depend on this being stable.
func TestValidateEdgeAttachedChecksInOrder(t *testing.T) {
	p := EdgeAttachedPayload{
		Confidence:       0,           // wrong
		Tier:             99,          // also wrong
		RelationshipType: "",          // also wrong
		ReviewState:      "CONFIRMED", // also wrong
	}
	if err := ValidateEdgeAttached(p); err != ErrConfidenceOutOfRange {
		t.Errorf("err = %v, want ErrConfidenceOutOfRange (checked first)", err)
	}
}

func TestHTTPErrorError(t *testing.T) {
	// Error() must NOT echo the response body verbatim (S-7): a server
	// that reflects a request payload back, or names the X-API-Key in
	// its debug output, would otherwise be re-leaked into logs and
	// crash dumps via the error message. Body is preserved as a field
	// for Classification() and for structured callers; only the
	// user-facing string is sanitized.
	e := &HTTPError{StatusCode: 400, Body: "bad request"}
	want := "branchdam: server returned HTTP 400"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if e.Body != "bad request" {
		t.Errorf("Body field unexpectedly mutated by Error() = %q, want \"bad request\"", e.Body)
	}
}

// problemJSON renders a Huma-style problem+json body, the shape every
// handler error from branchDAM's agent routes takes.
func problemJSON(status int, title, detail, code string) string {
	b, _ := json.Marshal(map[string]any{"title": title, "status": status, "detail": detail, "code": code})
	return string(b)
}

// TestHTTPErrorClassificationByStatus pins the per-status rule against the
// responses branchDAM's auth middleware and handleAgentRebase actually
// send. Before this table existed every status other than 429/5xx fell
// back to Fatal, so a rejected key, a misconfigured server (503) or a
// request timeout marked a queued rebase row permanently FAILED.
func TestHTTPErrorClassificationByStatus(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want Classification
	}{
		// Auth middleware (plain text): fixed by re-pairing / config, never by dropping the row.
		{"401 invalid key", 401, "invalid or missing X-API-Key", ClassificationTransient},
		{"401 bad signature", 401, "invalid or missing signature", ClassificationTransient},
		{"503 auth not configured", 503, "agent authentication is not configured", ClassificationTransient},
		{"403 machine principal required", 403, problemJSON(403, "Forbidden", "agent machine principal required", ""), ClassificationTransient},
		{"408 timeout", 408, "request timeout", ClassificationTransient},
		{"429 rate limited", 429, "slow down", ClassificationTransient},
		{"500 rebase tx failed", 500, problemJSON(500, "Internal Server Error", "rebase operation failed", ""), ClassificationTransient},
		{"500 even naming a constraint", 500, problemJSON(500, "Internal Server Error", "UNIQUE constraint failed: media_nodes.file_path", ""), ClassificationTransient},
		{"502 proxy", 502, "bad gateway", ClassificationTransient},
		{"504 proxy", 504, "", ClassificationTransient},
		{"unlisted 4xx", 405, "method not allowed", ClassificationTransient},
		// handleAgentRebase's own refusals (problem+json): permanent for this request.
		{"400 invalid nodeUuid", 400, problemJSON(400, "Bad Request", "nodeUuid must be a UUID", ""), ClassificationFatal},
		{"400 read-only tier", 400, problemJSON(400, "Bad Request", `rebase to read-only tier TIER1_SSD_CACHE refused for path "/x"`, ""), ClassificationFatal},
		{"400 invalid target path", 400, problemJSON(400, "Bad Request", `invalid target path "/nowhere": no storage location`, ""), ClassificationFatal},
		{"404 archived node", 404, problemJSON(404, "Not Found", "asset not found", ""), ClassificationFatal},
		{"409 hash conflict", 409, problemJSON(409, "Conflict", "node already has a different fullHash", ""), ClassificationFatal},
		{"422 schema violation", 422, `{"title":"Unprocessable Entity","status":422}`, ClassificationFatal},
		{"403 not owner", 403, problemJSON(403, "Forbidden", "node was not created by this device", ""), ClassificationFatal},
		// 400 whose message names a condition that clears by itself.
		{"400 tier-3 file not there yet", 400, problemJSON(400, "Bad Request", `rebase to read-only tier TIER3_MASTER_ARCHIVE refused for path "/a": no file exists there yet`, ""), ClassificationTransient},
		{"400 target stat failed", 400, problemJSON(400, "Bad Request", `checking whether rebase target "/a" already exists: input/output error`, ""), ClassificationTransient},
		{"400 body cut off (middleware)", 400, "failed to read request body", ClassificationTransient},
		// The same statuses as plain text did not come from the handler.
		{"404 plain text (route missing)", 404, "404 page not found", ClassificationTransient},
		{"400 plain text", 400, "bad request", ClassificationTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &HTTPError{StatusCode: tc.code, Body: tc.body}
			if got := e.Classification(); got != tc.want {
				t.Errorf("Classification() = %v, want %v", got, tc.want)
			}
			if got, want := e.Retryable(), tc.want != ClassificationFatal; got != want {
				t.Errorf("Retryable() = %v, want %v", got, want)
			}
		})
	}
}

// TestHTTPErrorClassificationPrefersCode: a recognized machine-readable
// code decides regardless of status or wording; an unrecognized one falls
// back to the status rules.
func TestHTTPErrorClassificationPrefersCode(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want Classification
	}{
		{"fatal code on otherwise-transient 403", 403, problemJSON(403, "Forbidden", "nope", "NODE_NOT_OWNER"), ClassificationFatal},
		{"fatal code on 500", 500, problemJSON(500, "Internal Server Error", "x", "HASH_CONFLICT"), ClassificationFatal},
		{"transient code on otherwise-fatal 400", 400, problemJSON(400, "Bad Request", "x", "ARCHIVE_FILE_NOT_YET_PRESENT"), ClassificationTransient},
		{"transient code on 404", 404, problemJSON(404, "Not Found", "media node not yet applied", "NODE_NOT_FOUND"), ClassificationTransient},
		{"auth code", 401, problemJSON(401, "Unauthorized", "x", "AUTH_INVALID_KEY"), ClassificationTransient},
		{"unknown code falls back to 409 rule", 409, problemJSON(409, "Conflict", "x", "SOMETHING_NEW"), ClassificationFatal},
		{"unknown code falls back to 503 rule", 503, problemJSON(503, "Service Unavailable", "x", "SOMETHING_NEW"), ClassificationTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &HTTPError{StatusCode: tc.code, Body: tc.body}
			if got := e.Classification(); got != tc.want {
				t.Errorf("Classification() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTTPErrorCode(t *testing.T) {
	if got := (&HTTPError{StatusCode: 409, Body: problemJSON(409, "Conflict", "x", "HASH_CONFLICT")}).Code(); got != "HASH_CONFLICT" {
		t.Errorf("Code() = %q, want HASH_CONFLICT", got)
	}
	for _, body := range []string{"", "plain text", `{"title":"Bad Request","status":400}`, `[1,2]`} {
		if got := (&HTTPError{StatusCode: 400, Body: body}).Code(); got != "" {
			t.Errorf("Code() for %q = %q, want empty", body, got)
		}
	}
}

// TestHTTPErrorClassificationNoGenericTransientHint: a genuine fatal
// refusal (a problem+json 404 from handleAgentRebase) stays fatal even when
// its detail happens to contain "not found" wording -- only the specific,
// 400-sourced phrases in transientBodyHints may override the status.
func TestHTTPErrorClassificationNoGenericTransientHint(t *testing.T) {
	e := &HTTPError{
		StatusCode: 404,
		Body:       `{"title":"Not Found","status":404,"detail":"media node not found"}`,
	}
	if got := e.Classification(); got != ClassificationFatal {
		t.Fatalf("Classification() = %v, want ClassificationFatal", got)
	}
}
