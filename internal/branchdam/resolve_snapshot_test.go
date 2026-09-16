package branchdam

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostResolveSnapshotSynchronousContract(t *testing.T) {
	var captured ResolveSnapshot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agent/resolve-snapshot" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Error("missing API key")
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode snapshot: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"refreshed":2,"removed":3,"unchanged":4,"unresolved":5,"reviewedConflicts":6}`))
	}))
	defer srv.Close()
	client := New(srv.URL, "test-key")
	response, err := client.PostResolveSnapshot(context.Background(), ResolveSnapshot{
		AgentID: "agent-a", ScopeID: "scope", Timelines: []ResolveSnapshotTimeline{},
		Memberships: []ResolveSnapshotMembership{},
	})
	if err != nil || response.Created != 1 || response.Refreshed != 2 || response.Removed != 3 || response.ReviewedConflicts != 6 {
		t.Fatalf("response = %+v, err = %v", response, err)
	}
	if captured.AgentID != "agent-a" || captured.ScopeID != "scope" || captured.Memberships == nil {
		t.Fatalf("request = %+v", captured)
	}
}

func TestPostResolveSnapshotRejectsMissingScope(t *testing.T) {
	client := New("http://localhost:8080", "test-key")
	if _, err := client.PostResolveSnapshot(context.Background(), ResolveSnapshot{AgentID: "agent-a"}); err == nil {
		t.Fatal("missing scopeId accepted")
	}
}
