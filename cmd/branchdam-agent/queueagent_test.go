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
