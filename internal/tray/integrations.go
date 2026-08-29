package tray

import (
	"context"
	"time"
)

// IntegrationID names one third-party catalog-reading integration -- a
// string, not an int enum, because it is also the config.yaml key segment
// ("integrations.<id>.enabled"), so the two can never independently drift
// once the Settings menu (a later PR) builds dotted keys from it.
type IntegrationID string

const (
	// IntegrationLuminar is Skylum Luminar Neo's catalog reader.
	IntegrationLuminar IntegrationID = "luminar"
	// Future entries: IntegrationLrcat ("lrcat", issue #47),
	// IntegrationApplePhotos ("applephotos", issue #46).
)

// IntegrationDescriptor is one compile-time registry entry -- what the
// (later) Settings menu needs to build its items from. onReady builds the
// menu once with no teardown-and-rebuild path, so this list must stay
// static and never config-driven; adding a new integration means appending
// an entry here, not touching menu-building code.
type IntegrationDescriptor struct {
	ID    IntegrationID
	Title string // e.g. "Luminar Neo"
}

// Integrations is the compile-time registry every catalog-sync integration
// is listed in, in menu/status-page order. A function, not an exported
// package var, matching this package's DefaultX/XOrDefault house style and
// keeping the list immutable to callers.
//
// DaVinci Resolve is deliberately NOT in this list: it has no
// CatalogSyncConfig and no IntegrationSyncer (see
// internal/config.IntegrationsConfig's own doc comment) -- it's an
// installer, not a sync integration, and gets its own seam in issue #60.
func Integrations() []IntegrationDescriptor {
	return []IntegrationDescriptor{
		{ID: IntegrationLuminar, Title: "Luminar Neo"},
	}
}

// SyncSummary is the tray-local mirror of internal/luminar.Stats, condensed
// to the fields a menu line and a status-page row need -- the same reason
// QueueCounts mirrors queue.Counts (see queue.go). Deliberately NOT
// Luminar-specific: a future lrcat (#47)/applephotos (#46) syncer reports
// the same shape. At is set by Runner.TriggerSync, not by the
// implementation, so it always reflects when the tray actually ran the
// pass, not when the underlying Sync call happened to return.
type SyncSummary struct {
	At         time.Time
	DryRun     bool
	PairsFound int
	Emitted    int // posted, or -- in a dry run -- that WOULD have been posted
	Skipped    int // candidates skipped because an endpoint had no node-index entry
	Errors     int // per-edge POST failures; the pass itself still completed
	Err        error
}

// IntegrationSyncer is the subset of one catalog integration's behavior the
// tray runs on its own timer and from a "Sync now" menu item (see
// Runner.TriggerSync) -- an interface, like Drainer/Pruner, so this package
// stays free of internal/luminar and unit-testable with a fake.
//
// An implementation MUST open and close its catalog handle inside Sync, per
// pass, rather than holding one for the tray's whole lifetime: the operator
// may have the source catalog open live in the third-party application
// right now, and re-opening per pass is also what lets a regenerated
// node-index file be picked up without a tray restart.
type IntegrationSyncer interface {
	Sync(ctx context.Context) (SyncSummary, error)
}

// IntegrationStatus is Runner's RUNTIME view of one integration -- paired
// with a (later PR's) Settings snapshot for the menu/status page to render
// "enabled but not configured" vs. "ready" vs. a real last-sync summary.
// Runner itself never reads config (see Reconfigure's own doc comment), so
// Registered=false is the honest "cmd/branchdam-agent did not wire a
// syncer for this ID" signal -- covering both "disabled" and "enabled but
// missing a required config field" -- mirroring QueueStatus.Configured.
type IntegrationStatus struct {
	ID         IntegrationID
	Registered bool
	LastSync   *SyncSummary
}
