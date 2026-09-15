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
// specifically to prevent that regression.
func TestJSONErrFieldsRenderAsStrings(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name string
		v    any
	}{
		{"IngestSummary", IngestSummary{Err: boom}},
		{"UpdateStatus", UpdateStatus{Err: boom}},
		{"DrainSummary", DrainSummary{Err: boom}},
		{"PruneSummary", PruneSummary{Err: boom}},
		{"QueueStatus", QueueStatus{Err: boom}},
		{"SyncSummary", SyncSummary{Err: boom}},
		{"HookState", HookState{Err: boom}},
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
			got, ok := decoded["Err"]
			if !ok {
				t.Fatalf("%s: no Err key in %s", tc.name, data)
			}
			str, ok := got.(string)
			if !ok {
				t.Fatalf("%s: Err = %#v (%T), want a string -- got raw JSON %s", tc.name, got, got, data)
			}
			if str != "boom" {
				t.Errorf("%s: Err = %q, want %q", tc.name, str, "boom")
			}
		})
	}
}

// TestJSONErrFieldOmittedWhenNil confirms the `omitempty` half of the same
// contract: no error means no Err key at all, not a null or empty-string
// one -- what every renderX function in app.js treats as "falsy, don't
// show an error pill" (`if (x.Err) …`).
func TestJSONErrFieldOmittedWhenNil(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"IngestSummary", IngestSummary{}},
		{"UpdateStatus", UpdateStatus{}},
		{"DrainSummary", DrainSummary{}},
		{"PruneSummary", PruneSummary{}},
		{"QueueStatus", QueueStatus{}},
		{"SyncSummary", SyncSummary{}},
		{"HookState", HookState{}},
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
			if _, ok := decoded["Err"]; ok {
				t.Errorf("%s: Err key present with a nil error: %s", tc.name, data)
			}
		})
	}
}

// TestIngestSummaryJSONKeysArePascalCase pins the (currently untagged, so
// PascalCase) key set app.js's renderIngest reads directly -- unlike every
// other field of Status, which has explicit camelCase json tags.
// Re-tagging IngestSummary to match would be a legitimate follow-up, but it
// must come with a matching app.js change; this test exists so that
// change can't land as a silent, one-sided drift in either direction.
func TestIngestSummaryJSONKeysArePascalCase(t *testing.T) {
	data, err := json.Marshal(IngestSummary{CardPath: "/mnt/card", Submitted: 3})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CardPath", "Submitted"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("expected PascalCase key %q in %s", key, data)
		}
	}
}
