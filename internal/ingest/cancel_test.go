package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
)

// stopOnFirstSubmitClient requests an operator stop from inside the first
// file's registration call -- i.e. while that file is in flight -- and
// records whether the ctx that call ran under was already canceled.
type stopOnFirstSubmitClient struct {
	fakeClient
	stop          context.CancelFunc
	ctxErrAtFirst error
}

func (c *stopOnFirstSubmitClient) PostNodeCreated(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload) (*branchdam.EventResponse, error) {
	if len(c.calls) == 0 {
		c.stop()
		c.ctxErrAtFirst = ctx.Err()
	}
	return c.fakeClient.PostNodeCreated(ctx, agentID, payload)
}

func (c *stopOnFirstSubmitClient) PostNodeCreatedWithUUID(ctx context.Context, agentID string, payload branchdam.NodeCreatedPayload, eventUUID string) (*branchdam.EventResponse, error) {
	if len(c.calls) == 0 {
		c.stop()
		c.ctxErrAtFirst = ctx.Err()
	}
	return c.fakeClient.PostNodeCreatedWithUUID(ctx, agentID, payload, eventUUID)
}

// writeNumberedCard creates cardRoot with n small JPEGs, IMG_0001.jpg onward.
func writeNumberedCard(t *testing.T, cardRoot string, n int) {
	t.Helper()
	if err := os.MkdirAll(cardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		name := filepath.Join(cardRoot, fmt.Sprintf("IMG_%04d.jpg", i))
		if err := os.WriteFile(name, []byte(fmt.Sprintf("photo-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIngestCardStopsAtFileBoundary pins the operator-stop contract: the
// file in flight when the stop arrives is copied AND registered under an
// uncanceled ctx, no later file is touched, and the partial result comes
// back with an error wrapping ErrCanceled.
func TestIngestCardStopsAtFileBoundary(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeNumberedCard(t, cardRoot, 3)

	ctx, stop := WithFileBoundaryCancel(context.Background())
	defer stop()
	client := &stopOnFirstSubmitClient{stop: stop}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))

	res, err := e.IngestCard(ctx, cardRoot)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("IngestCard err = %v, want it to wrap ErrCanceled", err)
	}
	if client.ctxErrAtFirst != nil {
		t.Errorf("in-flight file's registration ran under a canceled ctx (%v); an operator stop must let it finish", client.ctxErrAtFirst)
	}
	if len(res.Files) != 1 {
		t.Fatalf("partial result has %d files, want exactly the 1 in flight when the stop arrived", len(res.Files))
	}
	if res.Files[0].Err != nil || res.Files[0].EventID == "" {
		t.Errorf("in-flight file = %+v, want it registered without error", res.Files[0])
	}
	if len(client.calls) != 1 {
		t.Errorf("got %d registrations, want 1 -- no file after the stop may be processed", len(client.calls))
	}
	if _, statErr := os.Stat(filepath.Join(dir, "local", "IMG_0002.jpg")); !os.IsNotExist(statErr) {
		t.Errorf("IMG_0002.jpg was copied after the stop (stat err %v)", statErr)
	}
}

// TestIngestCardWithoutStopIngestsEverything guards the default: the stop
// value is inert until its cancel func is called.
func TestIngestCardWithoutStopIngestsEverything(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeNumberedCard(t, cardRoot, 3)

	ctx, stop := WithFileBoundaryCancel(context.Background())
	defer stop()
	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	res, err := e.IngestCard(ctx, cardRoot)
	if err != nil {
		t.Fatalf("IngestCard: %v", err)
	}
	if len(res.Files) != 3 || len(client.calls) != 3 {
		t.Fatalf("got %d files / %d registrations, want 3 / 3", len(res.Files), len(client.calls))
	}
}

// TestIngestCardParentCancelStopsWalk covers a shutdown: a canceled parent
// ctx ends the walk before the first entry and still returns a (here
// empty) partial result with an error wrapping context.Canceled, rather
// than a hard walk failure.
func TestIngestCardParentCancelStopsWalk(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeNumberedCard(t, cardRoot, 2)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &fakeClient{}
	e := newTestEngine(t, client, filepath.Join(dir, "archive"), filepath.Join(dir, "local"))
	res, err := e.IngestCard(ctx, cardRoot)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if errors.Is(err, ErrCanceled) {
		t.Errorf("a shutdown must not be reported as an operator stop: %v", err)
	}
	if len(res.Files) != 0 || len(client.calls) != 0 {
		t.Errorf("got %d files / %d registrations after a pre-canceled ctx, want none", len(res.Files), len(client.calls))
	}
}

// TestIngestCardOfflineStopsAtFileBoundary is the offline twin of
// TestIngestCardStopsAtFileBoundary: the stop lands after the first file's
// queue row is durable, and the walk queues nothing more.
func TestIngestCardOfflineStopsAtFileBoundary(t *testing.T) {
	dir := t.TempDir()
	cardRoot := filepath.Join(dir, "card")
	writeNumberedCard(t, cardRoot, 3)

	store := openStoreT(t)
	ctx, stop := WithFileBoundaryCancel(context.Background())
	defer stop()
	e := newOfflineTestEngine(t, failingClient{}, filepath.Join(dir, "archive-unreachable"), filepath.Join(dir, "local"), "/tier0", store)
	var seen int
	e.Progress = func(ev ProgressEvent) {
		if seen == 0 {
			stop()
		}
		seen++
	}

	res, err := e.IngestCardOffline(ctx, cardRoot)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("IngestCardOffline err = %v, want it to wrap ErrCanceled", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("partial result has %d files, want 1", len(res.Files))
	}
	if res.Files[0].Err != nil {
		t.Errorf("in-flight file failed: %v", res.Files[0].Err)
	}
	rows, err := store.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("queue has %d rows, want exactly the in-flight file's", len(rows))
	}
}
