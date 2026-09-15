package tray

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestJSONErrFieldsRenderAsStrings pins the contract cmd/branchdam-agent-ui's
// frontend depends on for every *.Err field GET /api/status and
// /status.json expose: a bare `error`-typed field marshals to "{}" by
// default (most error implementations expose no fields of their own), which
// would silently render as "[object Object]" in a JS consumer that treats
// a truthy Err as a string. Each type below has its own MarshalJSON
// specifically to prevent that regression. IngestSummary's error key is
// lowercase ("err") since that type is otherwise fully camelCase-tagged;
// every other type here is still untagged/PascalCase apart from its Err
// override (see each MarshalJSON's own doc comment).
func TestJSONErrFieldsRenderAsStrings(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name   string
		v      any
		errKey string
	}{
		{"IngestSummary", IngestSummary{Err: boom}, "err"},
		{"UpdateStatus", UpdateStatus{Err: boom}, "Err"},
		{"DrainSummary", DrainSummary{Err: boom}, "Err"},
		{"PruneSummary", PruneSummary{Err: boom}, "Err"},
		{"QueueStatus", QueueStatus{Err: boom}, "Err"},
		{"SyncSummary", SyncSummary{Err: boom}, "Err"},
		{"HookState", HookState{Err: boom}, "Err"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			got, ok := decoded[tc.errKey]
			if !ok {
				t.Fatalf("%s: no %q key in %s", tc.name, tc.errKey, data)
			}
			str, ok := got.(string)
			if !ok {
				t.Fatalf("%s: %s = %#v (%T), want a string -- got raw JSON %s", tc.name, tc.errKey, got, got, data)
			}
			if str != "boom" {
				t.Errorf("%s: %s = %q, want %q", tc.name, tc.errKey, str, "boom")
			}
		})
	}
}

// TestJSONErrFieldOmittedWhenNil confirms the `omitempty` half of the same
// contract: no error means no Err/err key at all, not a null or
// empty-string one -- what every renderX function in app.js treats as
// "falsy, don't show an error pill".
func TestJSONErrFieldOmittedWhenNil(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		errKey string
	}{
		{"IngestSummary", IngestSummary{}, "err"},
		{"UpdateStatus", UpdateStatus{}, "Err"},
		{"DrainSummary", DrainSummary{}, "Err"},
		{"PruneSummary", PruneSummary{}, "Err"},
		{"QueueStatus", QueueStatus{}, "Err"},
		{"SyncSummary", SyncSummary{}, "Err"},
		{"HookState", HookState{}, "Err"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if _, ok := decoded[tc.errKey]; ok {
				t.Errorf("%s: %s key present with a nil error: %s", tc.name, tc.errKey, data)
			}
		})
	}
}

// TestIngestSummaryJSONKeysAreCamelCase pins the camelCase key set app.js's
// renderIngest reads -- unlike this file's other summary/state types
// (DrainSummary, QueueStatus, etc.), which stay untagged/PascalCase apart
// from their Err override. IngestSummary is fully tagged because
// cmd/branchdam-agent-ui's frontend (added in the same PR that added these
// tags) is its only JSON consumer, making this the natural moment to align
// it with the rest of Status's already-camelCase surface. Asserting the
// old PascalCase keys are ABSENT, not just that the new ones are present,
// is what would catch a re-tag landing without the matching app.js change
// (or vice versa) -- see this repo's own review history for why that
// pairing keeps being the actual failure mode.
func TestIngestSummaryJSONKeysAreCamelCase(t *testing.T) {
	data, err := json.Marshal(IngestSummary{CardPath: "/mnt/card", Submitted: 3})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cardPath", "submitted"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("expected camelCase key %q in %s", key, data)
		}
	}
	for _, key := range []string{"CardPath", "Submitted"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("unexpected leftover PascalCase key %q in %s", key, data)
		}
	}
}
