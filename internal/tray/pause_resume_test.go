package tray

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordingGate is an IngestGate that records every volume it is asked
// about and declines, so no ingest runs.
type recordingGate struct {
	mu    sync.Mutex
	asked []string
}

func (g *recordingGate) Confirm(_ context.Context, volumePath, _ string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, volumePath)
	return false, nil
}

func (g *recordingGate) count(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.asked {
		if p == path {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestResumeOffersCardsInsertedWhilePaused is the regression test for a
// card inserted mid-pause: the detector reports it once, while paused, and
// before this fix it was dropped and never offered again.
func TestResumeOffersCardsInsertedWhilePaused(t *testing.T) {
	dir := t.TempDir()
	gate := &recordingGate{}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)
	r.SetIngestGate(gate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := filepath.Join(dir, "BEFORE")
	if err := os.Mkdir(before, 0o755); err != nil {
		t.Fatal(err)
	}
	r.ReconfigureDetector(ctx, []string{dir})
	defer r.StopDetector()
	if !waitFor(t, func() bool { return gate.count(before) == 1 }) {
		t.Fatal("card present at start was never offered")
	}

	r.SetPaused(true)
	during := filepath.Join(dir, "DURING")
	pulled := filepath.Join(dir, "PULLED")
	for _, d := range []string{during, pulled} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Let the detector see both insertions while paused, then pull one.
	if !waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.pausedInserts[during] && r.pausedInserts[pulled]
	}) {
		t.Fatal("detector never reported the cards inserted while paused")
	}
	if err := os.Remove(pulled); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.pausedInserts[pulled]
	}) {
		t.Fatal("detector never reported the pulled card as removed")
	}
	if n := gate.count(during); n != 0 {
		t.Fatalf("card offered %d times while paused, want 0", n)
	}

	r.SetPaused(false)
	if !waitFor(t, func() bool { return gate.count(during) == 1 }) {
		t.Fatalf("card inserted while paused was not offered through the IngestGate on resume (asked=%v)", gate.asked)
	}
	time.Sleep(50 * time.Millisecond)
	if n := gate.count(before); n != 1 {
		t.Errorf("card handled before the pause was offered %d times, want 1 (no re-offer on resume)", n)
	}
	if n := gate.count(pulled); n != 0 {
		t.Errorf("card removed while paused was offered %d times on resume, want 0", n)
	}
	if n := gate.count(during); n != 1 {
		t.Errorf("card inserted while paused offered %d times, want exactly 1", n)
	}
}

// TestDetectorRestartWhilePausedDoesNotReofferHandledCards: a reload that
// restarts the detector while paused (new roots or poll interval) must not
// report a card it already handled as a new insertion, so resuming offers
// nothing.
func TestDetectorRestartWhilePausedDoesNotReofferHandledCards(t *testing.T) {
	dir := t.TempDir()
	gate := &recordingGate{}
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetDetectorInterval(10 * time.Millisecond)
	r.SetIngestGate(gate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	card := filepath.Join(dir, "CARD")
	if err := os.Mkdir(card, 0o755); err != nil {
		t.Fatal(err)
	}
	r.ReconfigureDetector(ctx, []string{dir})
	defer r.StopDetector()
	if !waitFor(t, func() bool { return gate.count(card) == 1 }) {
		t.Fatal("card present at start was never offered")
	}

	r.SetPaused(true)
	r.ReconfigureDetector(ctx, []string{dir})
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	held := len(r.pausedInserts)
	r.mu.Unlock()
	if held != 0 {
		t.Errorf("restarted detector held %d already-handled cards, want 0", held)
	}

	r.SetPaused(false)
	time.Sleep(50 * time.Millisecond)
	if n := gate.count(card); n != 1 {
		t.Errorf("handled card offered %d times after a paused restart and resume, want 1", n)
	}
}

// TestPausedDetectionIsHeldNotDropped: a detection that reaches
// triggerIngest while paused (for example after a re-pause raced the
// resume offer) is held for the next resume instead of being dropped.
// A manual ingest is not held.
func TestPausedDetectionIsHeldNotDropped(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	r.SetPaused(true)
	ctx := context.Background()

	r.TriggerDetectedIngest(ctx, "/media/CARD")
	r.TriggerIngest(ctx, "/media/MANUAL")

	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.pausedInserts["/media/CARD"] {
		t.Error("detection while paused was dropped, want it held for resume")
	}
	if r.pausedInserts["/media/MANUAL"] {
		t.Error("manual ingest while paused was held, want it skipped")
	}
}
