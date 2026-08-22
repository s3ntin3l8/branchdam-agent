// conformance_test.go golden-file tests. These assert every payload struct's
// marshalled JSON matches a committed fixture in testdata/ byte-for-byte, so
// an accidental field rename, a dropped omitempty, or a wrong JSON tag fails
// loudly here instead of silently at the branchDAM server (422, or worse, a
// silently-ignored field). Fixtures are regenerated with:
//
//	go test ./internal/branchdam/... -run TestConformance -update
//
// which writes internal/branchdam/testdata/*.golden.json to match the
// structs' current marshalled output -- review the diff before committing a
// regenerated fixture; an unreviewed -update run defeats the point of this
// test.
package branchdam

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
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

// strPtr/int64Ptr/float64Ptr build pointer values inline, since every
// optional NodeCreatedPayload field beyond the first few is *T.
func strPtr(s string) *string       { return &s }
func int64Ptr(n int64) *int64       { return &n }
func float64Ptr(f float64) *float64 { return &f }

// TestConformanceNodeCreatedFull pins the full 19-field payload shape the
// plan's conformance contract calls out (internal/agent/types.go:51-84 in
// branchdam) -- StorageLocationID deliberately excluded, since it's
// deprecated/ignored server-side and the plan's own "Send:" field list omits
// it.
func TestConformanceNodeCreatedFull(t *testing.T) {
	p := NodeCreatedPayload{
		NodeUUID:           "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		FilePath:           "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC01234.ARW",
		FileName:           "DSC01234.ARW",
		FileExt:            ".ARW",
		SizeBytes:          52428800,
		MtimeUnix:          1752591000,
		FastHash:           strPtr("a17463135ce85764"),
		FullHash:           strPtr("d1b2c3a4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff"),
		Phash:              int64Ptr(-4611686018427387903),
		CameraModel:        strPtr("ILCE-7M4"),
		CameraSerial:       strPtr("1234567"),
		LensModel:          strPtr("FE 24-70mm F2.8 GM"),
		CapturedAtUnix:     int64Ptr(1752591000),
		OriginalDocumentID: strPtr("xmp.did:0190f1a2-orig"),
		DocumentID:         strPtr("xmp.did:0190f1a2-doc"),
		DerivedFromID:      strPtr("xmp.did:0190f1a2-parent"),
		FilenameStem:       strPtr("dsc01234"),
		GPSLatitude:        float64Ptr(-33.9151),
		GPSLongitude:       float64Ptr(18.4115),
	}
	checkGolden(t, "node_created_full.golden.json", marshalIndent(t, p))
}

// TestConformanceNodeCreatedRequiredOnly proves the omitempty asymmetry: the
// two always-on-the-wire fields (NodeUUID, FilePath) appear even at zero
// value, the four non-pointer omitempty fields (FileName, FileExt,
// SizeBytes, MtimeUnix) vanish entirely at zero value rather than
// serializing as "" / 0, and every pointer field stays nil -> absent.
func TestConformanceNodeCreatedRequiredOnly(t *testing.T) {
	p := NodeCreatedPayload{
		NodeUUID: "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		FilePath: "/storage/staging/agent-01/incoming/DSC01235.ARW",
	}
	checkGolden(t, "node_created_required_only.golden.json", marshalIndent(t, p))
}

// TestConformanceEdgeAttachedPayload pins EdgeAttachedPayload's own shape,
// with EvidenceJSON as a bare JSON *object* -- the opposite encoding from
// EventEnvelope.Payload (a JSON *string*), which
// TestConformanceEventEnvelopeDoubleEncoding below exercises in the same
// request so the two can never be confused by a future refactor.
func TestConformanceEdgeAttachedPayload(t *testing.T) {
	p := EdgeAttachedPayload{
		SourceNodeUUID:   "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		TargetNodeUUID:   "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f61",
		RelationshipType: RelationshipDerivedFrom,
		Confidence:       0.89,
		Tier:             2,
		Resolver:         "luminar_catalog",
		EvidenceJSON:     json.RawMessage(`{"catalogPath":"/Volumes/Edit/catalog.db","editId":42}`),
	}
	checkGolden(t, "edge_attached_payload.golden.json", marshalIndent(t, p))
}

// TestConformanceEventEnvelopeDoubleEncoding pins the full request body for
// POST /api/v1/agent/events when sending an EVENT_EDGE_ATTACHED: the
// envelope's own "payload" field is a JSON *string* containing the
// escaped, double-encoded EdgeAttachedPayload -- sending payload as a bare
// object is a 422 (AgentEventInput.Body.Payload is a Go string
// server-side). This is the single most confusable pair in the whole
// contract (per the plan): evidenceJson is an object one level in, payload
// is a string one level out.
func TestConformanceEventEnvelopeDoubleEncoding(t *testing.T) {
	edge := EdgeAttachedPayload{
		SourceNodeUUID:   "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		TargetNodeUUID:   "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f61",
		RelationshipType: RelationshipDerivedFrom,
		Confidence:       0.89,
		Tier:             2,
		Resolver:         "luminar_catalog",
		EvidenceJSON:     json.RawMessage(`{"catalogPath":"/Volumes/Edit/catalog.db","editId":42}`),
	}
	payloadJSON, err := json.Marshal(edge)
	if err != nil {
		t.Fatalf("marshal edge payload: %v", err)
	}
	env := EventEnvelope{
		AgentID:   "workstation-01",
		EventType: EventEdgeAttached,
		Payload:   string(payloadJSON),
	}
	checkGolden(t, "event_envelope_edge_attached.golden.json", marshalIndent(t, env))
}

func TestConformanceNodeMovedPayload(t *testing.T) {
	p := NodeMovedPayload{
		NodeUUID:    "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		NewFilePath: "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC01234-renamed.ARW",
		NewFileName: "DSC01234-renamed.ARW",
		MtimeUnix:   1752591100,
	}
	checkGolden(t, "node_moved_payload.golden.json", marshalIndent(t, p))
}

func TestConformanceNodeDeletedPayload(t *testing.T) {
	p := NodeDeletedPayload{NodeUUID: "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60"}
	checkGolden(t, "node_deleted_payload.golden.json", marshalIndent(t, p))
}

func TestConformancePathRebasedPayload(t *testing.T) {
	p := PathRebasedPayload{
		NodeUUID:       "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		TargetFilePath: "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC01234.ARW",
		TargetFileName: "DSC01234.ARW",
		MtimeUnix:      1752591000,
		FastHash:       strPtr("a17463135ce85764"),
		SizeBytes:      52428800,
	}
	checkGolden(t, "path_rebased_payload.golden.json", marshalIndent(t, p))
}

func TestConformanceHelloResponse(t *testing.T) {
	p := HelloResponse{OK: true, Version: "0.42.0"}
	checkGolden(t, "hello_response.golden.json", marshalIndent(t, p))
}

func TestConformanceHandshakeRequestResponse(t *testing.T) {
	req := HandshakeRequest{AgentID: "workstation-01"}
	checkGolden(t, "handshake_request.golden.json", marshalIndent(t, req))

	resp := HandshakeResponse{
		OK:                    true,
		ServerVersion:         "0.42.0",
		ServerTimeUnix:        1752591200,
		AcknowledgedEventUUID: "0190f1a2-9999-7000-8000-000000000000",
		PendingEventsCount:    3,
	}
	checkGolden(t, "handshake_response.golden.json", marshalIndent(t, resp))
}

func TestConformanceRebaseRequestResponse(t *testing.T) {
	req := RebaseRequest{
		NodeUUID:   "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		TargetPath: "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC01234.ARW",
		FileName:   "DSC01234.ARW",
		FileExt:    ".ARW",
		SizeBytes:  52428800,
		MtimeUnix:  1752591000,
		FastHash:   strPtr("a17463135ce85764"),
	}
	checkGolden(t, "rebase_request.golden.json", marshalIndent(t, req))

	resp := RebaseResponse{
		ID:                42,
		NodeUUID:          "0190f1a2-3b4c-7d5e-8f6a-1b2c3d4e5f60",
		StorageLocationID: 3,
		FilePath:          "/storage/archive/2026/2026-07-15_ILCE-7M4/DSC01234.ARW",
		Status:            "REBASED",
	}
	checkGolden(t, "rebase_response.golden.json", marshalIndent(t, resp))
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
