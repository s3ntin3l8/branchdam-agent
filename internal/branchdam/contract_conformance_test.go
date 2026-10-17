package branchdam

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The pinned copy of branchDAM's agent-API contract fixtures (X1):
// testdata/contract/*.json is copied verbatim from the server's
// docs/contract/fixtures (refresh with `make contract-sync`; SOURCE names
// the server commit). Each fixture sets every field the server reads or
// writes, so decoding one into this package's DTO and re-encoding it shows
// every field this client drops, renames or retypes.
const contractDir = "testdata/contract"

// contractCase maps one server fixture to the DTO this client uses for it.
// notModelled lists the fixture's fields this client deliberately does not
// carry, each with the reason -- anything else missing is drift.
type contractCase struct {
	newDTO      func() any
	notModelled map[string]string // JSON path -> why the client omits it
}

func contractCases() map[string]contractCase {
	return map[string]contractCase{
		"hello_response":     {newDTO: func() any { return &HelloResponse{} }},
		"handshake_request":  {newDTO: func() any { return &HandshakeRequest{} }},
		"handshake_response": {newDTO: func() any { return &HandshakeResponse{} }},
		"events_request":     {newDTO: func() any { return &EventEnvelope{} }},
		"events_response":    {newDTO: func() any { return &EventResponse{} }},

		"event_node_created":         {newDTO: func() any { return &NodeCreatedPayload{} }},
		"event_edge_attached":        {newDTO: func() any { return &EdgeAttachedPayload{} }},
		"event_node_moved":           {newDTO: func() any { return &NodeMovedPayload{} }},
		"event_node_deleted":         {newDTO: func() any { return &NodeDeletedPayload{} }},
		"event_path_rebased":         {newDTO: func() any { return &PathRebasedPayload{} }},
		"event_virtual_node_created": {newDTO: func() any { return &VirtualNodeCreated{} }},

		"rebase_request":      {newDTO: func() any { return &RebaseRequest{} }},
		"rebase_response":     {newDTO: func() any { return &RebaseResponse{} }},
		"node_status_request": {newDTO: func() any { return &NodeStatusRequest{} }},
		"node_status_response": {
			newDTO: func() any { return &NodeStatusResponse{} },
			notModelled: map[string]string{
				"statuses[].canonicalNodeUuid": "prune keys on the queue row's own nodeUuid and compares fullHash; " +
					"the alias target is informational for this client",
			},
		},
		"check_content_response":    {newDTO: func() any { return &ContentCheckResult{} }},
		"resolve_snapshot_request":  {newDTO: func() any { return &ResolveSnapshot{} }},
		"resolve_snapshot_response": {newDTO: func() any { return &ResolveSnapshotResponse{} }},
		"telemetry_request":         {newDTO: func() any { return &AgentTelemetryInput{} }},
		"telemetry_response":        {newDTO: func() any { return &AgentTelemetryOutput{} }},
		"upload_response":           {newDTO: func() any { return &UploadResponse{} }},
	}
}

// contractNotUsed lists fixtures this client has no DTO for, with why.
// check_content_query and upload_request_headers are checked against the
// real request the client sends (TestContractCheckContentQuery,
// TestContractUploadHeaders), not decoded.
var contractNotUsed = map[string]string{
	"source_status_query":    "the agent never calls GET /api/v1/agent/source-status",
	"source_status_response": "the agent never calls GET /api/v1/agent/source-status",
	"check_content_query":    "checked by TestContractCheckContentQuery",
	"upload_request_headers": "checked by TestContractUploadHeaders",
}

func readContractFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, name+".json"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b
}

// dropPath removes one notModelled path ("a.b", "list[].field") from a
// decoded JSON value.
func dropPath(v any, path []string) {
	if len(path) == 0 {
		return
	}
	key, isList := strings.CutSuffix(path[0], "[]")
	obj, ok := v.(map[string]any)
	if !ok {
		return
	}
	if len(path) == 1 && !isList {
		delete(obj, key)
		return
	}
	if isList {
		if list, ok := obj[key].([]any); ok {
			for _, el := range list {
				dropPath(el, path[1:])
			}
		}
		return
	}
	dropPath(obj[key], path[1:])
}

// TestContractFixturesRoundTripThroughClientDTOs: every fixture this client
// uses decodes into its DTO with no unknown field, and re-encodes to the
// same JSON, except for the documented notModelled fields. A field the
// server renamed, retyped, added or dropped fails here.
func TestContractFixturesRoundTripThroughClientDTOs(t *testing.T) {
	for name, tc := range contractCases() {
		t.Run(name, func(t *testing.T) {
			var want any
			if err := json.Unmarshal(readContractFixture(t, name), &want); err != nil {
				t.Fatal(err)
			}
			for path := range tc.notModelled {
				dropPath(want, strings.Split(path, "."))
			}
			stripped, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}

			dto := tc.newDTO()
			dec := json.NewDecoder(bytes.NewReader(stripped))
			dec.DisallowUnknownFields()
			if err := dec.Decode(dto); err != nil {
				t.Fatalf("server fixture has a field %T does not model (add it, or list it in notModelled with a reason): %v", dto, err)
			}
			again, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			if err := json.Unmarshal(again, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%T does not round-trip the server fixture:\n got  %s\n want %s", dto, again, stripped)
			}
		})
	}
}

// TestContractFixturesAllAccountedFor: every pinned fixture is either
// checked or listed as unused with a reason, so a new server route can't
// go unnoticed after a contract-sync.
func TestContractFixturesAllAccountedFor(t *testing.T) {
	entries, err := os.ReadDir(contractDir)
	if err != nil {
		t.Fatal(err)
	}
	cases := contractCases()
	var missing []string
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if _, ok := cases[stem]; ok {
			continue
		}
		if _, ok := contractNotUsed[stem]; ok {
			continue
		}
		missing = append(missing, stem)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("pinned server fixtures with no conformance check: %v", missing)
	}
	for name := range cases {
		if _, err := os.Stat(filepath.Join(contractDir, name+".json")); err != nil {
			t.Errorf("case %s has no pinned fixture: %v", name, err)
		}
	}
}

// TestContractCheckContentQuery: the query parameters CheckContent sends
// are exactly the server fixture's names, for each request shape the
// ingest engine makes: the full lookup, the fast pre-screen (no fullHash)
// and a lookup with no known size (sizeBytes omitted, not sent as 0).
func TestContractCheckContentQuery(t *testing.T) {
	var fx map[string]any
	if err := json.Unmarshal(readContractFixture(t, "check_content_query"), &fx); err != nil {
		t.Fatal(err)
	}
	fastHash, _ := fx["fastHash"].(string)
	fullHash, _ := fx["fullHash"].(string)
	sizeBytes, _ := fx["sizeBytes"].(float64)
	if fastHash == "" || fullHash == "" || sizeBytes <= 0 {
		t.Fatalf("check_content_query fixture lacks fastHash, fullHash or sizeBytes: %v", fx)
	}

	cases := []struct {
		name string
		q    ContentCheckQuery
		want []string
	}{
		{"full", ContentCheckQuery{FastHash: fastHash, FullHash: fullHash, SizeBytes: int64(sizeBytes)}, []string{"fastHash", "fullHash", "sizeBytes"}},
		{"pre-screen", ContentCheckQuery{FastHash: fastHash, SizeBytes: int64(sizeBytes)}, []string{"fastHash", "sizeBytes"}},
		{"unknown size", ContentCheckQuery{FastHash: fastHash, FullHash: fullHash}, []string{"fastHash", "fullHash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery map[string][]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				_, _ = w.Write([]byte(`{"found":false,"possibleMatch":false}`))
			}))
			defer srv.Close()

			if _, err := New(srv.URL, "test-key").CheckContent(context.Background(), tc.q); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.want {
				want := fx[name]
				if f, ok := want.(float64); ok {
					want = strconv.FormatInt(int64(f), 10)
				}
				if got := gotQuery[name]; len(got) != 1 || got[0] != want {
					t.Errorf("query %s = %v, want [%v]", name, got, want)
				}
			}
			for name := range gotQuery {
				if _, ok := fx[name]; !ok {
					t.Errorf("CheckContent sends query %s, which the server fixture does not have", name)
				}
			}
			if len(gotQuery) != len(tc.want) {
				t.Errorf("CheckContent sent %v, want exactly %v", gotQuery, tc.want)
			}
		})
	}
}

// TestContractSourceMatchesContractVersion: ContractVersion names the same
// branchDAM commit testdata/contract/SOURCE records, so the client never
// advertises one server commit while its fixtures prove another.
func TestContractSourceMatchesContractVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(contractDir, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 || fields[0] != "branchdam" {
		t.Fatalf("SOURCE = %q, want \"branchdam <sha> <subject>\"", b)
	}
	if want := "branchdam@" + fields[1]; ContractVersion != want {
		t.Errorf("ContractVersion = %q, want %q (SOURCE's commit); run make contract-sync", ContractVersion, want)
	}
}

// TestContractUploadHeaders: Upload sends every header the server's
// upload route reads, under the fixture's names.
func TestContractUploadHeaders(t *testing.T) {
	var fx map[string]string
	if err := json.Unmarshal(readContractFixture(t, "upload_request_headers"), &fx); err != nil {
		t.Fatal(err)
	}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(readContractFixture(t, "upload_response"))
	}))
	defer srv.Close()

	ts, err := strconv.ParseInt(fx["X-Capture-Timestamp"], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	opts := UploadOptions{
		Filename:         fx["X-Filename"],
		CameraModel:      fx["X-Camera-Model"],
		CaptureTimestamp: ts,
		Blake3Hash:       fx["X-Blake3-Hash"],
		SourcePathHash:   fx["X-Source-Path-Hash"],
		SizeBytes:        4,
	}
	if _, err := New(srv.URL, "test-key").Upload(context.Background(), strings.NewReader("data"), opts); err != nil {
		t.Fatal(err)
	}
	for name, want := range fx {
		if v := got.Get(name); v != want {
			t.Errorf("header %s = %q, want %q", name, v, want)
		}
	}
}
