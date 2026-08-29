package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// TestRegistryCompleteness is the single highest-value test in this file:
// it ranges tray.Integrations() (the presentation registry) and asserts
// every entry has a matching integrationBuilders (the execution registry)
// entry, in the same order, by ID. Without this, adding lrcat (#47) or
// applephotos (#46) to one registry and forgetting the other renders a
// dead menu item (or, worse, a syncer nothing ever surfaces) instead of
// failing a test.
func TestRegistryCompleteness(t *testing.T) {
	descriptors := tray.Integrations()
	if len(descriptors) != len(integrationBuilders) {
		t.Fatalf("tray.Integrations() has %d entries, integrationBuilders has %d -- they must be in bijection", len(descriptors), len(integrationBuilders))
	}
	for i, d := range descriptors {
		b := integrationBuilders[i]
		if d.ID != b.ID {
			t.Errorf("index %d: tray.Integrations()[%d].ID = %q, integrationBuilders[%d].ID = %q -- registries must list IDs in the same order", i, i, d.ID, i, b.ID)
		}
		if b.Ready == nil || b.New == nil || b.Interval == nil {
			t.Errorf("integrationBuilders[%d] (%q) is missing Ready/New/Interval", i, b.ID)
		}
	}
}

func TestBuildIntegrationDepsLuminarReadiness(t *testing.T) {
	client := branchdam.New("http://example.invalid", "0123456789abcdef0123456789abcdef")

	cases := []struct {
		name string
		cfg  config.IntegrationsConfig
		want bool
	}{
		{"disabled", config.IntegrationsConfig{}, false},
		{
			"enabled but no catalogPath",
			config.IntegrationsConfig{Luminar: config.CatalogSyncConfig{Enabled: true}},
			false,
		},
		{
			"enabled, dry run, no node index -- still ready (dry run needs no index)",
			config.IntegrationsConfig{Luminar: config.CatalogSyncConfig{Enabled: true, CatalogPath: "/c.db", DryRun: true}},
			true,
		},
		{
			"enabled, live, no node index -- not ready",
			config.IntegrationsConfig{Luminar: config.CatalogSyncConfig{Enabled: true, CatalogPath: "/c.db", DryRun: false}},
			false,
		},
		{
			"enabled, live, with node index -- ready",
			config.IntegrationsConfig{
				NodeIndexPath: "/n.json",
				Luminar:       config.CatalogSyncConfig{Enabled: true, CatalogPath: "/c.db", DryRun: false},
			},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := buildIntegrationDeps(config.Config{Integrations: tc.cfg}, client)
			_, got := deps[tray.IntegrationLuminar]
			if got != tc.want {
				t.Errorf("registered = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLuminarSyncerDryRun(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	s := &luminarSyncer{
		agentID:       "test-agent",
		catalogPath:   catalogPath,
		nodeIndexPath: nodeIndexPath,
		dryRun:        true,
		timeout:       10 * time.Second,
	}

	summary, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !summary.DryRun {
		t.Error("expected DryRun=true on the summary")
	}
	if summary.PairsFound != 1 || summary.Emitted != 1 {
		t.Errorf("got %+v, want 1 pair found and 1 (would-be) emitted", summary)
	}
}

func TestLuminarSyncerLiveEmitsToRealServer(t *testing.T) {
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	var gotPayload branchdam.EdgeAttachedPayload
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/events", func(w http.ResponseWriter, r *http.Request) {
		var env branchdam.EventEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if err := json.Unmarshal([]byte(env.Payload), &gotPayload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"eventId":"evt-tray-sync"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := branchdam.New(srv.URL, "0123456789abcdef0123456789abcdef")
	s := &luminarSyncer{
		client:        client,
		agentID:       "test-agent",
		catalogPath:   catalogPath,
		nodeIndexPath: nodeIndexPath,
		dryRun:        false,
		timeout:       10 * time.Second,
	}

	summary, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if summary.DryRun {
		t.Error("expected DryRun=false")
	}
	if summary.Emitted != 1 {
		t.Errorf("got Emitted=%d, want 1", summary.Emitted)
	}
	if gotPayload.SourceNodeUUID != "0198f2c1-2e3a-7c9e-8b1a-000000000001" {
		t.Errorf("SourceNodeUUID = %q, want the master's uuid -- luminarSyncer did not reach the real server", gotPayload.SourceNodeUUID)
	}
}

func TestLuminarSyncerClosesCatalogHandle(t *testing.T) {
	// Regression guard for the "never hold a handle across the tray's
	// lifetime" invariant (tray.IntegrationSyncer's own doc comment):
	// running Sync twice in a row against the same catalog path must
	// succeed both times -- if the first call leaked an open *sql.DB
	// (SQLite locks the file, or simply leaks a descriptor), a second
	// Open would either fail or silently reuse stale state.
	dir := t.TempDir()
	catalogPath := createTestCatalog(t, dir)
	nodeIndexPath := writeNodeIndexFile(t, dir)

	s := &luminarSyncer{
		agentID:       "test-agent",
		catalogPath:   catalogPath,
		nodeIndexPath: nodeIndexPath,
		dryRun:        true,
		timeout:       10 * time.Second,
	}

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("second Sync: %v -- suggests the first call left the catalog handle open", err)
	}
}

// TestStartPeriodicVarReReadsInterval proves the interval is re-read
// LIVE on every check, not captured once at start -- the whole reason
// startPeriodicVar exists instead of reusing queueagent.go's startPeriodic
// (whose ticker DOES capture its interval once, which is why
// offline.drainIntervalSecs is documented config-file-only). Starts with
// interval()==0 ("manual only"/not yet configured), confirms nothing fires
// for a few check cycles, then flips interval() to a short positive value
// and confirms a fire follows -- all within one goroutine's lifetime, i.e.
// with no restart.
func TestStartPeriodicVarReReadsInterval(t *testing.T) {
	var mu sync.Mutex
	iv := time.Duration(0)
	getInterval := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return iv
	}
	setInterval := func(v time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		iv = v
	}

	calls := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go startPeriodicVar(ctx, 10*time.Millisecond, getInterval, time.Second, func(_ context.Context) {
		select {
		case calls <- struct{}{}:
		default:
		}
	})

	select {
	case <-calls:
		t.Fatal("expected no fire while interval() returns 0")
	case <-time.After(100 * time.Millisecond):
		// Expected: several check ticks passed with interval()==0.
	}

	setInterval(20 * time.Millisecond)

	select {
	case <-calls:
		// Expected: interval() flipped positive and startPeriodicVar
		// picked it up on its next check, with no restart.
	case <-time.After(2 * time.Second):
		t.Fatal("expected startPeriodicVar to fire once interval() turned positive, without a restart")
	}
}

func TestStartPeriodicVarIdlesOnNonPositiveInterval(t *testing.T) {
	calls := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go startPeriodicVar(ctx, 10*time.Millisecond, func() time.Duration {
		return 0 // "manual only" / not configured
	}, time.Second, func(_ context.Context) {
		select {
		case calls <- struct{}{}:
		default:
		}
	})

	select {
	case <-calls:
		t.Fatal("expected startPeriodicVar never to fire fn while interval() returns <= 0")
	case <-time.After(200 * time.Millisecond):
		// Expected: several checkInterval ticks passed with no fire.
	}
}
