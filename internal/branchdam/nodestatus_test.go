package branchdam

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestClientNodeStatusPostsPathAndReturnsStatuses(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody NodeStatusRequest
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"statuses":[
			{"nodeUuid":"u1","found":true,"lifecycleState":"ACTIVE","tier":"TIER3_MASTER_ARCHIVE","verified":true,"fullHash":"b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a"},
			{"nodeUuid":"u2","found":false,"verified":false}
		]}`))
	})
	_ = srv

	resp, err := c.NodeStatus(context.Background(), NodeStatusRequest{NodeUUIDs: []string{"u1", "u2"}})
	if err != nil {
		t.Fatalf("NodeStatus: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/agent/node-status" {
		t.Errorf("path = %q, want /api/v1/agent/node-status", gotPath)
	}
	if len(gotBody.NodeUUIDs) != 2 || gotBody.NodeUUIDs[0] != "u1" || gotBody.NodeUUIDs[1] != "u2" {
		t.Errorf("request body NodeUUIDs = %+v", gotBody.NodeUUIDs)
	}

	if len(resp.Statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(resp.Statuses))
	}
	if !resp.Statuses[0].Found || !resp.Statuses[0].Verified || resp.Statuses[0].Tier != "TIER3_MASTER_ARCHIVE" {
		t.Errorf("statuses[0] = %+v, want found+verified on TIER3_MASTER_ARCHIVE", resp.Statuses[0])
	}
	if resp.Statuses[0].FullHash != "b3f1c4d9e2a7568013c9a4d2e8f7b1063c5a9d7e2f4b8016938ac1d4e7f2b09a" {
		t.Errorf("statuses[0].FullHash = %q, want the server's fullHash", resp.Statuses[0].FullHash)
	}
	if resp.Statuses[1].Found || resp.Statuses[1].FullHash != "" {
		t.Errorf("statuses[1] = %+v, want Found=false and no fullHash", resp.Statuses[1])
	}
}

func TestClientNodeStatusOversizedBatchRefused400(t *testing.T) {
	srv, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"title":"nodeUuids must not exceed 400 per request"}`))
	})
	_ = srv

	uuids := make([]string, 201)
	for i := range uuids {
		uuids[i] = "u"
	}
	_, err := c.NodeStatus(context.Background(), NodeStatusRequest{NodeUUIDs: uuids})
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if httpErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", httpErr.StatusCode)
	}
}
