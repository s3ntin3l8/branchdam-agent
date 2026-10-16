package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// TestConfigSettingsConcurrentSavesKeepEveryChange: saves arriving
// together (the Settings window and the tray menu) must each land, on
// disk and in the snapshot.
func TestConfigSettingsConcurrentSavesKeepEveryChange(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	keys := []string{
		"tray.confirmDestructive",
		"ingest.requireUnbuffered",
		"ingest.requireDCIM",
		"ingest.pauseUploadOnMetered",
		"ingest.autoEject",
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(keys)+1)
	for _, k := range keys {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			errs <- s.SetBool(k, true)
		}(k)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- s.SetInt("selfUpdate.checkIntervalHours", 7)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	onDisk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]config.Config{"on disk": onDisk, "in memory": s.currentConfig()} {
		got := []bool{c.Tray.ConfirmDestructive, c.Ingest.RequireUnbuffered, c.Ingest.RequireDCIM, c.Ingest.PauseUploadOnMetered, c.Ingest.AutoEject}
		if slices.Contains(got, false) || c.SelfUpdate.CheckIntervalHours != 7 {
			t.Errorf("%s: lost a concurrent save: bools=%v checkIntervalHours=%d", name, got, c.SelfUpdate.CheckIntervalHours)
		}
	}
}

// TestConfigSettingsSnapshotNamesRestartRequiredKeys: a saved change to a
// key only read at startup is flagged, by name, until reverted.
func TestConfigSettingsSnapshotNamesRestartRequiredKeys(t *testing.T) {
	path, cfg, runner := settingsTestFixture(t)
	s := newConfigSettings(path, cfg, runner)

	if err := config.Patch(path, map[string]any{
		"ingest.pollIntervalSecs":       9,
		"offline.drainIntervalSecs":     17,
		"selfUpdate.checkIntervalHours": 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	sv := s.Snapshot()
	want := []string{"offline.drainIntervalSecs", "ingest.pollIntervalSecs", "selfUpdate.checkIntervalHours"}
	if !sv.RestartRequired {
		t.Error("RestartRequired = false after changing restart-only keys")
	}
	for _, k := range want {
		if !slices.Contains(sv.RestartRequiredKeys, k) {
			t.Errorf("RestartRequiredKeys = %v, missing %q", sv.RestartRequiredKeys, k)
		}
	}

	// A hot-reloadable change alone is not flagged; reverting clears it.
	if err := config.Patch(path, map[string]any{
		"ingest.pollIntervalSecs":       cfg.Ingest.PollIntervalSecs,
		"offline.drainIntervalSecs":     cfg.Offline.DrainIntervalSecs,
		"selfUpdate.checkIntervalHours": cfg.SelfUpdate.CheckIntervalHours,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBool("ingest.autoEject", true); err != nil {
		t.Fatal(err)
	}
	if sv := s.Snapshot(); sv.RestartRequired || len(sv.RestartRequiredKeys) != 0 {
		t.Errorf("after revert: RestartRequired=%v keys=%v, want false and none", sv.RestartRequired, sv.RestartRequiredKeys)
	}
}

func TestRestartRequiredKeysPruneEnableOnly(t *testing.T) {
	var off, on config.Config
	on.Prune.Enabled = true
	if got := restartRequiredKeys(off, on); !slices.Equal(got, []string{"prune.enabled"}) {
		t.Errorf("enabling prune: %v, want [prune.enabled]", got)
	}
	if got := restartRequiredKeys(on, off); len(got) != 0 {
		t.Errorf("disabling prune is live, got %v", got)
	}
	var q config.Config
	q.Offline.QueueDBPath = "/q.db"
	if got := restartRequiredKeys(off, q); !slices.Equal(got, []string{"offline.queueDbPath"}) {
		t.Errorf("queue path: %v", got)
	}
}

// heldIngester blocks its first IngestCard until release is closed.
type heldIngester struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldIngester) IngestCard(_ context.Context, _ string) (ingest.CardResult, error) {
	h.once.Do(func() {
		close(h.started)
		<-h.release
	})
	return ingest.CardResult{}, nil
}

func (h *heldIngester) IngestCardOffline(_ context.Context, _ string) (ingest.OfflineCardResult, error) {
	return ingest.OfflineCardResult{}, nil
}

// TestConfigSettingsSaveDuringIngestReturnsPromptly: a save while a card
// is mid-copy returns at once (the Settings window's 10s request used to
// time out behind the ingest and report the agent as not running), with
// the change on disk and ApplyPending set until the ingest finishes.
func TestConfigSettingsSaveDuringIngestReturnsPromptly(t *testing.T) {
	path, cfg, _ := settingsTestFixture(t)
	held := &heldIngester{started: make(chan struct{}), release: make(chan struct{})}
	runner := tray.NewRunner(held, nil, cfg.Ingest.LocalEditRoot)
	s := newConfigSettings(path, cfg, runner)

	ingestDone := make(chan struct{})
	go func() {
		runner.TriggerIngest(context.Background(), t.TempDir())
		close(ingestDone)
	}()
	<-held.started
	released := false
	release := func() {
		if !released {
			released = true
			close(held.release)
			<-ingestDone
		}
	}
	t.Cleanup(release)

	saved := make(chan error, 1)
	go func() { saved <- s.SetBool("ingest.requireUnbuffered", true) }()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatalf("SetBool: %v", err)
		}
	case <-time.After(8 * time.Second):
		release()
		t.Fatal("SetBool blocked behind the in-flight ingest")
	}

	if onDisk, err := config.Load(path); err != nil || !onDisk.Ingest.RequireUnbuffered {
		t.Errorf("change not on disk (err %v)", err)
	}
	if !s.Snapshot().ApplyPending {
		t.Error("ApplyPending = false while the ingest still runs")
	}

	release()
	deadline := time.Now().Add(2 * time.Second)
	for s.Snapshot().ApplyPending && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Snapshot().ApplyPending {
		t.Error("ApplyPending still set after the ingest finished")
	}
}
