package branchdam

import "testing"

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

func TestClassifyErrorFatalSubstrings(t *testing.T) {
	cases := []string{
		"malformed event payload: missing nodeUuid",
		"unknown event type: EVENT_FOO",
		"invalid or empty node_uuid",
		"rebase target resolves to read-only tier",
		"node is archived, refusing to rebase",
		"edge would create a cycle",
		"CHECK constraint failed: media_edges",
	}
	for _, msg := range cases {
		if got := ClassifyError(msg); got != ClassificationFatal {
			t.Errorf("ClassifyError(%q) = %v, want ClassificationFatal", msg, got)
		}
	}
}

func TestClassifyErrorTransientSubstrings(t *testing.T) {
	cases := []string{
		"media node not found",
		"rebase target resolves to tier 3 but the file does not exist there yet",
	}
	for _, msg := range cases {
		if got := ClassifyError(msg); got != ClassificationTransient {
			t.Errorf("ClassifyError(%q) = %v, want ClassificationTransient", msg, got)
		}
	}
}

func TestClassifyErrorCaseInsensitive(t *testing.T) {
	if got := ClassifyError("MALFORMED EVENT PAYLOAD"); got != ClassificationFatal {
		t.Errorf("ClassifyError uppercase = %v, want ClassificationFatal", got)
	}
}

func TestClassifyErrorUnknown(t *testing.T) {
	if got := ClassifyError("something completely unrelated happened"); got != ClassificationUnknown {
		t.Errorf("ClassifyError(unrelated) = %v, want ClassificationUnknown", got)
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

func TestHTTPErrorRetryable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{401, false},
		{403, false},
		{503, false},
		{429, true},
		{500, true},
		{502, true},
		{503 + 100, true}, // arbitrary 5xx above 503 itself
		{400, false},
		{404, false},
		{422, false},
	}
	for _, tc := range cases {
		e := &HTTPError{StatusCode: tc.code}
		if got := e.Retryable(); got != tc.want {
			t.Errorf("Retryable() for %d = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestHTTPErrorClassificationPrefersBodyOverStatus(t *testing.T) {
	// A 400 (normally fatal by status alone) whose body doesn't match any
	// known substring falls back to the coarse Retryable()-derived
	// classification; a 200-range status never reaches here in practice, but
	// a 5xx whose body happens to name a known-transient condition should
	// still classify by the body match, not just Retryable().
	e := &HTTPError{StatusCode: 500, Body: "media node not found"}
	if got := e.Classification(); got != ClassificationTransient {
		t.Errorf("Classification() = %v, want ClassificationTransient (body match wins)", got)
	}

	e2 := &HTTPError{StatusCode: 500, Body: "some other 500 error with no known substring"}
	if got := e2.Classification(); got != ClassificationTransient {
		t.Errorf("Classification() = %v, want ClassificationTransient (falls back to Retryable())", got)
	}

	e3 := &HTTPError{StatusCode: 400, Body: "some other 400 error with no known substring"}
	if got := e3.Classification(); got != ClassificationFatal {
		t.Errorf("Classification() = %v, want ClassificationFatal (falls back to !Retryable())", got)
	}
}
