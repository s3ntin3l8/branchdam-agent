package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/queue"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

func TestQueueCountsReaderMirrorsStoreCounts(t *testing.T) {
	store := openTestQueueStore(t)
	if err := store.InsertPending(context.Background(), queue.NewRecord{
		NodeUUID: "n1", Kind: queue.KindMedia, SourcePath: "/a", LocalPath: "/a",
		ArchivePath: "/archive/a", FileName: "a", FileExt: ".jpg",
	}); err != nil {
		t.Fatal(err)
	}

	reader := &queueCountsReader{store: store}
	got, err := reader.Counts(context.Background())
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if got.AwaitingUpload != 1 {
		t.Errorf("got %+v, want AwaitingUpload=1", got)
	}
}

func TestQueueDrainerWrapsIngestDrain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)

	d := &queueDrainer{client: client, store: store, agentID: "agent-01"}
	summary, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !summary.HandshakeOK {
		t.Error("expected HandshakeOK=true to carry through from ingest.DrainStats")
	}
}

func TestQueueDrainerStampsLastHandshakeAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)

	d := &queueDrainer{client: client, store: store, agentID: "agent-01"}
	before := time.Now()
	summary, err := d.Drain(context.Background())
	after := time.Now()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if summary.LastHandshakeAt.IsZero() {
		t.Fatal("expected LastHandshakeAt to be stamped on a successful drain pass")
	}
	if summary.LastHandshakeAt.Before(before) || summary.LastHandshakeAt.After(after) {
		t.Errorf("LastHandshakeAt=%v not within drain pass window [%v, %v]", summary.LastHandshakeAt, before, after)
	}
}

func TestQueueDrainerDoesNotStampLastHandshakeAtOnFailedHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"serverVersion":"0.5.0","serverTimeUnix":1,"pendingEventsCount":0}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)

	d := &queueDrainer{client: client, store: store, agentID: "agent-01"}
	summary, err := d.Drain(context.Background())
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if summary.HandshakeOK {
		t.Fatalf("expected HandshakeOK=false to carry through from a server-side ok:false response, got %+v", summary)
	}
	if !summary.LastHandshakeAt.IsZero() {
		t.Errorf("expected LastHandshakeAt to remain zero on a failed handshake, got %v", summary.LastHandshakeAt)
	}
}

func TestQueuePrunerWrapsPrunePass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"statuses":[]}`))
	}))
	defer srv.Close()

	client := branchdam.New(srv.URL, "test-key")
	store := openTestQueueStore(t)
	cfg := config.Config{
		Prune:  config.PruneConfig{Enabled: true},
		Ingest: config.IngestConfig{LocalEditRoot: t.TempDir()},
	}

	p := &queuePruner{client: client, store: store, cfg: cfg}
	summary, err := p.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if summary.Pruned != 0 {
		t.Errorf("got %+v, want a zero-row pass over an empty queue", summary)
	}
}

func TestStartPeriodicRunsOnTickerAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32

	done := make(chan struct{})
	go func() {
		startPeriodic(ctx, 10*time.Millisecond, time.Second, func(_ context.Context) {
			atomic.AddInt32(&calls, 1)
		})
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startPeriodic did not return after ctx cancellation")
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("expected at least one tick to have called fn before cancellation")
	}
}

func TestStartPeriodicDisabledForNonPositiveInterval(t *testing.T) {
	called := false
	done := make(chan struct{})
	go func() {
		startPeriodic(context.Background(), 0, time.Second, func(_ context.Context) {
			called = true
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startPeriodic with interval<=0 must return immediately, not block forever")
	}
	if called {
		t.Error("expected fn never to be called when interval<=0")
	}
}

// TestQueueCountsReaderFailedRowsAndRetry: the tray's failed-rows list maps
// each FAILED step to its reason, and RetryFailed resets one row or all of
// them through the queue store's own reset.
func TestQueueCountsReaderFailedRowsAndRetry(t *testing.T) {
	ctx := context.Background()
	store := openTestQueueStore(t)
	for _, u := range []string{"copy", "rebase", "other"} {
		if err := store.InsertPending(ctx, queue.NewRecord{
			NodeUUID: u, Kind: queue.KindMedia, SourcePath: "/card/" + u, LocalPath: "/local/" + u,
			ArchivePath: "/archive/" + u, FileName: u, FileExt: ".jpg",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkArchiveCopyFailed(ctx, "copy", "archive conflict"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchiveCopyDone(ctx, "rebase"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRebaseFailed(ctx, "rebase", "node is archived"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkArchiveCopyFailed(ctx, "other", "edited local copy"); err != nil {
		t.Fatal(err)
	}

	reader := &queueCountsReader{store: store}
	rows, err := reader.FailedRows(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("FailedRows = %d rows, want 3", len(rows))
	}
	if rows[0].Step != tray.FailedStepArchiveCopy || rows[0].LastError != "archive conflict" || rows[0].SourcePath != "/card/copy" {
		t.Errorf("row 0 = %+v, want the archive-copy failure", rows[0])
	}
	if rows[1].Step != tray.FailedStepRebase || rows[1].LastError != "node is archived" {
		t.Errorf("row 1 = %+v, want the rebase failure", rows[1])
	}

	c, err := reader.RetryFailed(ctx, "rebase")
	if err != nil || c != (tray.QueueRetryCounts{Rebases: 1}) {
		t.Fatalf("RetryFailed(rebase) = %+v, %v", c, err)
	}
	c, err = reader.RetryFailed(ctx, "")
	if err != nil || c != (tray.QueueRetryCounts{ArchiveCopies: 2}) {
		t.Fatalf("RetryFailed(all) = %+v, %v; want the two remaining archive copies", c, err)
	}
	if rows, _ := reader.FailedRows(ctx, 0); len(rows) != 0 {
		t.Errorf("rows still failed after retrying all: %+v", rows)
	}
}
