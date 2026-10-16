package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// gatedNodeClient blocks the first registration until release is closed,
// so a test can act while an ingest is provably mid-card.
type gatedNodeClient struct {
	mu       sync.Mutex
	calls    int
	inFlight chan struct{}
	release  chan struct{}
}

func (c *gatedNodeClient) PostNodeCreatedWithUUID(_ context.Context, _ string, p branchdam.NodeCreatedPayload, _ string) (*branchdam.EventResponse, error) {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.mu.Unlock()
	if first {
		close(c.inFlight)
		<-c.release
	}
	return &branchdam.EventResponse{EventID: "evt-" + p.NodeUUID}, nil
}

func newCancelTestEngine(t *testing.T, client *gatedNodeClient) (*ingest.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	card := filepath.Join(dir, "card")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(filepath.Join(card, fmt.Sprintf("IMG_%04d.jpg", i)), []byte(fmt.Sprintf("photo-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(dir, "archive")
	n := 0
	return &ingest.Engine{
		Client:  client,
		AgentID: "test-agent",
		Ingest: config.IngestConfig{
			ArchiveRoot:   archive,
			LocalEditRoot: filepath.Join(dir, "local"),
			PathTemplate:  "{original_name}",
		},
		Mappings: []config.PathMapping{{WorkstationPath: archive, ContainerPath: "/storage/archive"}},
		Exiftool: &ingest.Exiftool{},
		Now:      time.Now,
		NewNodeUUID: func() (string, error) {
			n++
			return fmt.Sprintf("uuid-%d", n), nil
		},
	}, card
}

// TestCancelIngestStopsAtFileBoundary drives a real ingest.Engine through
// Runner.TriggerIngest and cancels it while the first file is in flight:
// that file still completes, nothing after it is ingested, the summary
// reports Canceled (so OK() is false and the card is never auto-ejected),
// and Status() shows the pending stop while the file finishes.
func TestCancelIngestStopsAtFileBoundary(t *testing.T) {
	client := &gatedNodeClient{inFlight: make(chan struct{}), release: make(chan struct{})}
	engine, card := newCancelTestEngine(t, client)
	r := NewRunner(engine, nil, "")

	if r.CancelIngest() {
		t.Fatal("CancelIngest() = true with no ingest running, want false")
	}

	done := make(chan IngestSummary, 1)
	go func() { done <- r.TriggerIngest(context.Background(), card) }()
	<-client.inFlight

	if !r.CancelIngest() {
		t.Fatal("CancelIngest() = false while an ingest is in flight, want true")
	}
	if st := r.Status(UpdateStatus{}); !st.IngestCancelRequested || !st.Busy {
		t.Errorf("Status() Busy=%v IngestCancelRequested=%v mid-stop, want both true", st.Busy, st.IngestCancelRequested)
	}
	close(client.release)
	summary := <-done

	if !summary.Canceled || !errors.Is(summary.Err, ingest.ErrCanceled) {
		t.Fatalf("summary = %+v, want Canceled with an ErrCanceled error", summary)
	}
	if summary.Submitted != 1 {
		t.Errorf("Submitted = %d, want 1 (the file in flight finishes)", summary.Submitted)
	}
	if summary.OK() {
		t.Error("a canceled ingest reported OK(); it must never count as a finished card")
	}
	if client.calls != 1 {
		t.Errorf("registrations = %d, want 1 -- nothing after the stop", client.calls)
	}
	if r.CancelIngest() {
		t.Error("CancelIngest() = true after the ingest returned, want false")
	}
	if st := r.Status(UpdateStatus{}); st.IngestCancelRequested || st.Busy {
		t.Errorf("Status() after the stop: Busy=%v IngestCancelRequested=%v, want both false", st.Busy, st.IngestCancelRequested)
	}
	if st := r.Status(UpdateStatus{}); st.LastIngest == nil || !st.LastIngest.Canceled {
		t.Errorf("LastIngest = %+v, want it to record the cancel", st.LastIngest)
	}
}

// failureQueueReader is a QueueReader that also implements
// QueueFailureManager.
type failureQueueReader struct {
	fakeQueueReader
	rows      []FailedQueueRow
	retried   []string
	hadCtxDDL bool
}

func (f *failureQueueReader) FailedRows(ctx context.Context, limit int) ([]FailedQueueRow, error) {
	_, f.hadCtxDDL = ctx.Deadline()
	if limit > 0 && limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}

func (f *failureQueueReader) RetryFailed(_ context.Context, nodeUUID string) (QueueRetryCounts, error) {
	f.retried = append(f.retried, nodeUUID)
	return QueueRetryCounts{Rebases: 1}, nil
}

func TestRunnerFailedQueueRows(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	if _, err := r.FailedQueueRows(context.Background(), 0); !errors.Is(err, ErrQueueNotConfigured) {
		t.Errorf("no queue: err = %v, want ErrQueueNotConfigured", err)
	}
	if _, err := r.RetryFailedQueueRows(context.Background(), "x"); !errors.Is(err, ErrQueueNotConfigured) {
		t.Errorf("no queue: retry err = %v, want ErrQueueNotConfigured", err)
	}

	r.SetQueueDeps(&fakeQueueReader{}, nil, nil)
	if _, err := r.FailedQueueRows(context.Background(), 0); !errors.Is(err, ErrQueueRetryUnsupported) {
		t.Errorf("counts-only reader: err = %v, want ErrQueueRetryUnsupported", err)
	}

	q := &failureQueueReader{rows: []FailedQueueRow{{NodeUUID: "a"}, {NodeUUID: "b"}}}
	r.SetQueueDeps(q, nil, nil)
	rows, err := r.FailedQueueRows(context.Background(), 1)
	if err != nil || len(rows) != 1 || rows[0].NodeUUID != "a" {
		t.Errorf("FailedQueueRows(1) = %+v, %v; want [a]", rows, err)
	}
	if !q.hadCtxDDL {
		t.Error("FailedQueueRows passed an unbounded ctx; a wedged queue.db must not hang the handler")
	}
	if c, err := r.RetryFailedQueueRows(context.Background(), "b"); err != nil || c.Rebases != 1 {
		t.Errorf("RetryFailedQueueRows = %+v, %v", c, err)
	}
	if len(q.retried) != 1 || q.retried[0] != "b" {
		t.Errorf("retried = %v, want [b]", q.retried)
	}
}

func newWindowActionServer(a *spyActions) *http.ServeMux {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: a}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	return mux
}

func postIngestStart(t *testing.T, mux *http.ServeMux, cardPath string) (int, ingestStartResult) {
	t.Helper()
	body, _ := json.Marshal(ingestStartRequest{CardPath: cardPath})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/ingest-start", body))
	var got ingestStartResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

func receivePaths(t *testing.T, ch chan string, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		select {
		case p := <-ch:
			got = append(got, p)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d of %d ingests", len(got), n)
		}
	}
	return got
}

// TestIngestStartIngestsEveryDetectedVolume: an empty cardPath is the
// window's "Ingest now" -- every IngestNowVolumes entry, each through
// TriggerIngest, answered with 202 before the batch finishes.
func TestIngestStartIngestsEveryDetectedVolume(t *testing.T) {
	a := &spyActions{nowVolumes: []string{"/Volumes/A", "/Volumes/B"}, ingested: make(chan string, 2)}
	code, got := postIngestStart(t, newWindowActionServer(a), "")
	if code != http.StatusAccepted || !got.Started {
		t.Fatalf("ingest-start = %d %+v, want 202 started", code, got)
	}
	if paths := receivePaths(t, a.ingested, 2); paths[0] != "/Volumes/A" || paths[1] != "/Volumes/B" {
		t.Errorf("ingested %v, want [/Volumes/A /Volumes/B] in order", paths)
	}
}

// TestIngestStartStopsBatchOnCancel: one cancel stops the whole batch, not
// only the volume it landed on.
func TestIngestStartStopsBatchOnCancel(t *testing.T) {
	a := &spyActions{
		nowVolumes:    []string{"/Volumes/A", "/Volumes/B"},
		ingested:      make(chan string, 2),
		ingestSummary: IngestSummary{Canceled: true},
	}
	mux := newWindowActionServer(a)
	if code, got := postIngestStart(t, mux, ""); code != http.StatusAccepted || !got.Started {
		t.Fatalf("ingest-start = %d %+v", code, got)
	}
	receivePaths(t, a.ingested, 1)
	select {
	case p := <-a.ingested:
		t.Errorf("batch kept going after a canceled volume: ingested %s", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestIngestStartImportsPickedFolder(t *testing.T) {
	dir := t.TempDir()
	a := &spyActions{ingested: make(chan string, 1)}
	code, got := postIngestStart(t, newWindowActionServer(a), dir)
	if code != http.StatusAccepted || !got.Started {
		t.Fatalf("ingest-start(folder) = %d %+v, want 202 started", code, got)
	}
	if p := receivePaths(t, a.ingested, 1); p[0] != dir {
		t.Errorf("ingested %v, want [%s]", p, dir)
	}
}

// TestIngestStartRefusals: every refusal is a 200 with started=false and a
// reason, and none of them reaches TriggerIngest.
func TestIngestStartRefusals(t *testing.T) {
	watched := t.TempDir()
	for _, tc := range []struct {
		name     string
		actions  *spyActions
		cardPath string
	}{
		{"paused", &spyActions{paused: true, nowVolumes: []string{"/Volumes/A"}}, ""},
		{"config incomplete", &spyActions{configIncomplete: true, nowVolumes: []string{"/Volumes/A"}}, ""},
		{"already busy", &spyActions{busy: true, nowVolumes: []string{"/Volumes/A"}}, ""},
		{"update holds the gate", &spyActions{gateHeld: true, nowVolumes: []string{"/Volumes/A"}}, ""},
		{"relative folder", &spyActions{}, "DCIM"},
		{"folder with ..", &spyActions{}, watched + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(watched)},
		{"no card found", &spyActions{watchDirs: []string{"/Volumes"}}, ""},
		{"watched parent folder", &spyActions{watchDirs: []string{watched}}, watched},
		{"missing folder", &spyActions{}, filepath.Join(watched, "nope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.actions.ingested = make(chan string, 1)
			code, got := postIngestStart(t, newWindowActionServer(tc.actions), tc.cardPath)
			if code != http.StatusOK || got.Started || got.Err == "" {
				t.Errorf("ingest-start = %d %+v, want 200 started=false with a reason", code, got)
			}
			select {
			case p := <-tc.actions.ingested:
				t.Errorf("a refused start still ingested %s", p)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestHandleActionCancelIngest(t *testing.T) {
	a := &spyActions{cancelResult: true}
	rec := httptest.NewRecorder()
	newWindowActionServer(a).ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/cancel-ingest", nil))
	var got cancelIngestActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !got.Canceled || a.cancelCalls != 1 {
		t.Errorf("cancel-ingest = %d %+v (calls %d), want 200 canceled once", rec.Code, got, a.cancelCalls)
	}
}

func TestHandleQueueFailed(t *testing.T) {
	a := &spyActions{failedRows: []FailedQueueRow{{NodeUUID: "n1", Step: FailedStepRebase, LastError: "node is archived"}}}
	mux := newWindowActionServer(a)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/queue/failed", nil))
	var got queueFailedResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !got.Configured || len(got.Rows) != 1 || got.Rows[0].LastError != "node is archived" {
		t.Errorf("GET /api/queue/failed = %d %+v", rec.Code, got)
	}
	if a.failedLimit != failedRowsDefaultLimit {
		t.Errorf("limit = %d, want the default %d", a.failedLimit, failedRowsDefaultLimit)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/queue/failed?limit=5", nil))
	if rec.Code != http.StatusOK || a.failedLimit != 5 {
		t.Errorf("?limit=5 = %d, limit %d", rec.Code, a.failedLimit)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/queue/failed?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("?limit=0 = %d, want 400", rec.Code)
	}

	a.failedErr = ErrQueueNotConfigured
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodGet, "/api/queue/failed", nil))
	got = queueFailedResult{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Configured || got.Rows == nil {
		t.Errorf("unconfigured queue = %d %+v, want 200 configured=false rows=[]", rec.Code, got)
	}
}

func TestHandleActionQueueRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
		wantNode *string
	}{
		{"one row", `{"nodeUuid":"n1"}`, http.StatusOK, ptr("n1")},
		{"all rows", `{"all":true}`, http.StatusOK, ptr("")},
		{"empty body is not all", `{}`, http.StatusBadRequest, nil},
		{"both is ambiguous", `{"nodeUuid":"n1","all":true}`, http.StatusBadRequest, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &spyActions{retryCounts: QueueRetryCounts{Rebases: 1}}
			rec := httptest.NewRecorder()
			newWindowActionServer(a).ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/queue-retry", []byte(tc.body)))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantCode)
			}
			switch {
			case tc.wantNode == nil && a.retryNodeUUID != nil:
				t.Errorf("a rejected request reached RetryFailedQueueRows(%q)", *a.retryNodeUUID)
			case tc.wantNode != nil && (a.retryNodeUUID == nil || *a.retryNodeUUID != *tc.wantNode):
				t.Errorf("RetryFailedQueueRows got %v, want %q", a.retryNodeUUID, *tc.wantNode)
			}
		})
	}

	a := &spyActions{retryErr: ErrQueueNotConfigured}
	rec := httptest.NewRecorder()
	newWindowActionServer(a).ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/queue-retry", []byte(`{"all":true}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured queue retry = %d, want 503", rec.Code)
	}
}

func ptr(s string) *string { return &s }

type fakeRollbacker struct {
	started bool
	calls   int
}

func (f *fakeRollbacker) StartRollback() (UpdateStatus, bool) {
	f.calls++
	return UpdateStatus{Phase: UpdatePhaseRollingBack, RollbackVersion: "1.0.0"}, f.started
}

func TestHandleActionRollbackUpdate(t *testing.T) {
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok"}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/rollback-update", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no rollbacker = %d, want 503", rec.Code)
	}

	rb := &fakeRollbacker{started: true}
	s = &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", UpdateRollback: rb}
	mux = http.NewServeMux()
	s.registerAPIRoutes(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, "/api/actions/rollback-update", nil))
	var got rollbackUpdateActionResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !got.Started || got.Status.Phase != UpdatePhaseRollingBack || rb.calls != 1 {
		t.Errorf("rollback-update = %d %+v (calls %d)", rec.Code, got, rb.calls)
	}
}

// deadlineDrainActions records whether the drain/prune handlers bounded
// the pass they started.
type deadlineDrainActions struct {
	spyActions
	drainDeadline, pruneDeadline bool
}

func (a *deadlineDrainActions) TriggerDrain(ctx context.Context) (DrainSummary, bool) {
	_, a.drainDeadline = ctx.Deadline()
	return DrainSummary{}, true
}

func (a *deadlineDrainActions) TriggerPrune(ctx context.Context) (PruneSummary, bool) {
	_, a.pruneDeadline = ctx.Deadline()
	return PruneSummary{}, true
}

// TestDrainPruneHandlersAreBounded: the window's "Drain now"/"Prune now"
// detach from the request, so the handler itself must bound the pass the
// way the tray menu's click does (drainPruneClickTimeout).
func TestDrainPruneHandlersAreBounded(t *testing.T) {
	a := &deadlineDrainActions{}
	s := &StatusServer{Addr: "127.0.0.1:38080", Token: "tok", Actions: a}
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)
	for _, path := range []string{"/api/actions/drain", "/api/actions/prune"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newAuthedRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	if !a.drainDeadline || !a.pruneDeadline {
		t.Errorf("deadline set: drain=%v prune=%v, want both bounded", a.drainDeadline, a.pruneDeadline)
	}
}
