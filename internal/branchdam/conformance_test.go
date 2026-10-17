// conformance_test.go holds the client-authored wire checks that the
// vendored server fixtures (testdata/contract/, checked by
// contract_conformance_test.go) cannot express. Those fixtures set every
// field the server reads or writes, so their strict round-trip already pins
// every DTO's field names, and the type of every field a fixture sets (a
// decode-only response field the fixture leaves at its zero value is
// pinned by name only); what they cannot show is what this
// client leaves OFF the wire when an optional field is unset, or a request
// shape the server has no fixture for. Only those live here -- a golden that
// merely re-pins a fixture's full shape is a second copy that rots on the
// next DTO change (issue #331).
//
// The remaining golden fixtures are regenerated with:
//
//	go test ./internal/branchdam/... -run 'TestConformance|TestContractVersionSnapshot' -update
//
// which writes internal/branchdam/testdata/*.golden.* to match the structs'
// current marshalled output -- review the diff before committing a
// regenerated fixture; an unreviewed -update run defeats the point of this
// test.
package branchdam

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

var update = flag.Bool("update", false, "update golden fixtures in testdata/")

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	got = append(bytes.TrimRight(got, "\n"), '\n')
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
	}
	// Git's Windows checkout may materialize text fixtures with CRLF while
	// encoding/json consistently emits LF. Compare the JSON contract, not the
	// checkout's line-ending policy.
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func marshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// int64Ptr builds a pointer value inline for the optional *int64 fields.
func int64Ptr(n int64) *int64 { return &n }

// TestConformanceHandshakeRequestWithCurrentKeyID pins the wire shape of a
// handshake request with CurrentKeyID present, as a separate fixture from
// TestConformanceHandshakeRequestResponse's CurrentKeyID-absent case
// above. The server accepts and ignores the field (see
// HandshakeRequest.CurrentKeyID's doc comment); no caller sets it.
func TestConformanceHandshakeRequestWithCurrentKeyID(t *testing.T) {
	req := HandshakeRequest{AgentID: "workstation-01", CurrentKeyID: int64Ptr(5)}
	checkGolden(t, "handshake_request_with_key.golden.json", marshalIndent(t, req))
}

// TestRequestDTOsOmitUnsetOptionalFields pins, for every DTO this client
// encodes, exactly which keys go on the wire when only the required fields
// are set. The contract fixtures set every field, so their round-trip cannot
// see an omitempty that was dropped (a deprecated storageLocationId sent as
// 0, an unresolved Resolve membership sent with sourceNodeUuid "") or one
// that was wrongly added to a required field (a missing key is a 422). This
// replaces the per-DTO minimal-shape goldens (node_created_required_only,
// handshake_request, and the unset-optional halves of the other request
// goldens).
func TestRequestDTOsOmitUnsetOptionalFields(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want []string
	}{
		// The two always-on-the-wire fields appear; the four non-pointer
		// omitempty fields (fileName, fileExt, sizeBytes, mtimeUnix) and the
		// deprecated storageLocationId vanish at zero value rather than
		// serializing as "" / 0, and every pointer field stays nil -> absent.
		{"NodeCreatedPayload", NodeCreatedPayload{
			NodeUUID: "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
			FilePath: "/storage/staging/agent-01/incoming/DSC01235.ARW",
		}, []string{"filePath", "nodeUuid"}},
		// sourceNodeId/targetNodeId and the deprecated reviewState must never
		// be sent unset (a reviewState of "" is harmless, CONFIRMED is fatal).
		{"EdgeAttachedPayload", EdgeAttachedPayload{
			SourceNodeUUID: "a", TargetNodeUUID: "b", RelationshipType: RelationshipDerivedFrom,
		}, []string{"relationshipType", "sourceNodeUuid", "targetNodeUuid"}},
		{"NodeMovedPayload", NodeMovedPayload{NodeUUID: "u", NewFilePath: "/x"}, []string{"newFilePath", "nodeUuid"}},
		{"NodeDeletedPayload", NodeDeletedPayload{NodeUUID: "u"}, []string{"nodeUuid"}},
		{"PathRebasedPayload", PathRebasedPayload{NodeUUID: "u", TargetFilePath: "/y"}, []string{"nodeUuid", "targetFilePath"}},
		{"VirtualNodeCreated", VirtualNodeCreated{
			NodeUUID: "u", FilePath: "/virtual/resolve/w/x", DisplayName: "d", ProjectType: "resolve_project",
		}, []string{"displayName", "filePath", "nodeUuid", "projectType"}},
		{"EventEnvelope", EventEnvelope{AgentID: "w", EventType: EventNodeDeleted, Payload: "{}"}, []string{"agentId", "eventType", "payload"}},
		// clientVersion, lastProcessedEventUuid and currentKeyId are all
		// absent when unset; handshake_request_with_key.golden.json pins the
		// currentKeyId-present case.
		{"HandshakeRequest", HandshakeRequest{AgentID: "workstation-01"}, []string{"agentId"}},
		// fullHash and the deprecated storageLocationId are absent when unset.
		{"RebaseRequest", RebaseRequest{NodeUUID: "u", TargetPath: "/t"}, []string{"nodeUuid", "targetPath"}},
		{"ResolveSnapshot", ResolveSnapshot{
			AgentID: "w", ScopeID: "s", Timelines: []ResolveSnapshotTimeline{}, Memberships: []ResolveSnapshotMembership{},
		}, []string{"agentId", "memberships", "scopeId", "timelines"}},
		// An unresolved membership (media the agent has no node for) carries
		// no sourceNodeUuid and no evidenceJson at all.
		{"ResolveSnapshotMembership", ResolveSnapshotMembership{
			TimelineID: "timeline-1", MediaFilePath: `D:\Videos\unresolved.mov`,
		}, []string{"mediaFilePath", "timelineId"}},
		// A timeline has no optional field: every key is sent even when its
		// value is zero.
		{"ResolveSnapshotTimeline", ResolveSnapshotTimeline{},
			[]string{"displayName", "evidenceJson", "filePath", "nodeUuid", "timelineId"}},
		{"NodeStatusRequest", NodeStatusRequest{NodeUUIDs: []string{"u"}}, []string{"nodeUuids"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(b, &obj); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(obj))
			for k := range obj {
				got = append(got, k)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s with only required fields set encodes keys %v, want %v (%s)", tc.name, got, tc.want, b)
			}
		})
	}
}

// TestContractEdgeAttachedEventDoubleEncoding sends the server's
// event_edge_attached fixture through PostEdgeAttached and checks the
// request body against the server's events_request envelope: the
// envelope's "payload" is a JSON *string* (a bare object is a 422 --
// AgentEventInput.Body.Payload is a Go string server-side), and inside that
// string evidenceJson is still a JSON *object*. This is the single most
// confusable pair in the contract -- evidenceJson is an object one level
// in, payload is a string one level out -- and the round-trip test only
// sees each half separately. TestClientPostEventDoubleEncodesPayload covers
// the same for EVENT_NODE_CREATED.
func TestContractEdgeAttachedEventDoubleEncoding(t *testing.T) {
	fixture := readContractFixture(t, "event_edge_attached")
	var edge EdgeAttachedPayload
	if err := json.Unmarshal(fixture, &edge); err != nil {
		t.Fatal(err)
	}
	var envFixture map[string]any
	if err := json.Unmarshal(readContractFixture(t, "events_request"), &envFixture); err != nil {
		t.Fatal(err)
	}

	// Read every fixture on the test goroutine: the handler runs on the
	// server's goroutine, where t.Fatal must not be called.
	eventsResponse := readContractFixture(t, "events_response")
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(eventsResponse)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "test-key").PostEdgeAttached(context.Background(), "workstation-01", edge); err != nil {
		t.Fatal(err)
	}

	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	for k := range envFixture {
		if _, ok := env[k]; !ok {
			t.Errorf("envelope lacks %q, which the server's events_request fixture has", k)
		}
	}
	for k := range env {
		if _, ok := envFixture[k]; !ok {
			t.Errorf("envelope sends %q, which the server's events_request fixture does not have", k)
		}
	}
	if env["eventType"] != EventEdgeAttached {
		t.Errorf("eventType = %v, want %s", env["eventType"], EventEdgeAttached)
	}
	payload, ok := env["payload"].(string)
	if !ok {
		t.Fatalf("payload is %T, want a JSON string (double-encoded): %s", env["payload"], body)
	}
	var inner, want map[string]any
	if err := json.Unmarshal([]byte(payload), &inner); err != nil {
		t.Fatalf("payload string is not JSON: %v", err)
	}
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	if _, ok := inner["evidenceJson"].(map[string]any); !ok {
		t.Errorf("inner evidenceJson is %T, want a JSON object", inner["evidenceJson"])
	}
	if !reflect.DeepEqual(inner, want) {
		t.Errorf("double-encoded payload differs from the server fixture:\n got  %s\n want %s", payload, fixture)
	}
}

// TestHelloVsHandshakeFieldNamesDiffer is the regression test for the
// single most likely copy-paste bug in this client: hello returns "version",
// handshake returns "serverVersion" for the same concept. A shared struct
// (or a field rename that silently unified them) would make one of the two
// always decode empty against the real server.
func TestHelloVsHandshakeFieldNamesDiffer(t *testing.T) {
	helloJSON := marshalIndent(t, HelloResponse{OK: true, Version: "0.42.0"})
	if bytes.Contains(helloJSON, []byte("serverVersion")) {
		t.Error("HelloResponse must not contain serverVersion -- that's HandshakeResponse's field name")
	}
	if !bytes.Contains(helloJSON, []byte(`"version"`)) {
		t.Error("HelloResponse must contain \"version\"")
	}

	handshakeJSON := marshalIndent(t, HandshakeResponse{OK: true, ServerVersion: "0.42.0", ServerTimeUnix: 1})
	if !bytes.Contains(handshakeJSON, []byte("serverVersion")) {
		t.Error("HandshakeResponse must contain serverVersion")
	}
}

// TestContractVersionSnapshot pins the branchDAM contract version this
// client was built against (issue #62's AC). Bump ContractVersion and this
// golden together, deliberately, whenever branchdam's agent DTOs change --
// never let this drift silently.
func TestContractVersionSnapshot(t *testing.T) {
	checkGolden(t, "contract_version.golden.txt", []byte(ContractVersion))
}
